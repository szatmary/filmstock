package build

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/szatmary/filmstock"
	"github.com/szatmary/filmstock/internal/dump"
	"github.com/szatmary/filmstock/internal/wikitext"
)

// Pass 1: the dump, into the intermediate.
//
// This does no resolution of any kind. It does not decide whether a person is
// credited, whether a season belongs to a series, or whether a cast link points
// at a real article. Every one of those needs the whole corpus, and a streaming
// pass has, by definition, only the page in front of it.
//
// The recognisers are unchanged — buildFilm, buildEvent, buildBiography,
// buildSchedule and the television parser all already take a page and return
// what they made of it. What changes is that their output is stored rather than
// merged, and that nothing is thrown away for not being wanted yet.

func CmdImport(args []string) {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	dumps := fs.String("dumps", "dump", "directory holding the dumps")
	out := fs.String("inter", defaultInterPath(), "intermediate store to write")
	workers := fs.Int("workers", 18, "parallel parsers")
	noText := fs.Bool("no-wikitext", false, "skip storing wikitext (smaller, but a parser fix then needs the dump again)")
	limit := fs.Int("limit", 0, "stop after this many pages (0 = all); for smoke tests")
	incr := fs.String("incr", "", "apply one adds-changes dump instead of a full dump")
	fs.Parse(args)

	if *incr != "" {
		importIncr(*incr, *out, *workers, !*noText)
		return
	}
	d, err := findDumps(*dumps)
	if err != nil {
		fatal(err)
	}
	in, err := OpenInter(*out)
	if err != nil {
		fatal(err)
	}
	defer in.Close()
	if err := in.SetSource(d.articles); err != nil {
		fatal(err)
	}

	start := time.Now()
	var scanned, kept int64
	// The same reader extract uses: the multistream index lets each bz2 block be
	// decompressed and decoded on its own worker, so recognising runs in parallel
	// too. Streaming the dump as one bz2 and decoding serially puts every parser
	// behind a single core — measured at 470 pages/s against this path's
	// thousands, the difference between hours and minutes.
	//
	// The write stays serial because SQLite is; pages reach it over a channel so
	// the workers never block on each other.
	pages := make(chan *Page, 4096)
	var wg sync.WaitGroup
	wg.Add(1)
	var writeErr error
	go func() {
		defer wg.Done()
		for p := range pages {
			if writeErr != nil {
				continue // drain, so the producer never blocks on a dead writer
			}
			if err := in.Put(p); err != nil {
				writeErr = err
			}
		}
	}()

	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-tick.C:
				s, k := atomic.LoadInt64(&scanned), atomic.LoadInt64(&kept)
				el := time.Since(start).Seconds()
				fmt.Fprintf(os.Stderr, "  [%5.0fs] pages=%d (%.0f/s) kept=%d\n",
					el, s, float64(s)/el, k)
			case <-done:
				return
			}
		}
	}()

	var progress atomic.Int64
	var shouldStop func() bool
	if *limit > 0 {
		shouldStop = func() bool { return atomic.LoadInt64(&scanned) >= int64(*limit) }
	}
	err = dump.RunMultistreamProgress(d.articles, d.index, *workers, func(p dump.Page) {
		atomic.AddInt64(&scanned, 1)
		if p.NS != 0 {
			return
		}
		for _, ip := range recognise(p, !*noText) {
			atomic.AddInt64(&kept, 1)
			pages <- ip
		}
	}, shouldStop, &progress)
	close(pages)
	wg.Wait()
	close(done)
	if err != nil {
		fatal(err)
	}
	if writeErr != nil {
		fatal(writeErr)
	}
	if err := in.Flush(); err != nil {
		fatal(err)
	}

	counts, err := in.Counts()
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "\nimported %d of %d pages in %.1f min\n",
		atomic.LoadInt64(&kept), atomic.LoadInt64(&scanned), time.Since(start).Minutes())
	for _, k := range []string{filmstock.KindMovie, filmstock.KindTelevision,
		kindSeason, kindEpisodeList, filmstock.KindPerson, filmstock.KindEvent,
		filmstock.KindSchedule} {
		if counts[k] > 0 {
			fmt.Fprintf(os.Stderr, "  %-12s %8d\n", k, counts[k])
		}
	}
	if fi, err := os.Stat(*out); err == nil {
		fmt.Fprintf(os.Stderr, "  %-12s %8.1f GB\n", "store", float64(fi.Size())/(1<<30))
	}
}

// kindSeason is a season article, which the current pipeline folds into a series
// rather than keeping. Seasons carry a network, a cast that changes between
// them, and per-season ratings, none of which a series-level record can express.
const kindSeason = "seasons"

// kindEpisodeList is a "List of X episodes" article. It carries most of the
// corpus's 572,847 episodes and states no series of its own, so it is kept as a
// page and joined during export.
const kindEpisodeList = "episode_lists"

// recognise returns every entity a page yields. A page may yield more than one:
// an article can carry both a film infobox and a person infobox, and choosing
// between them needs the whole corpus.
func recognise(p dump.Page, keepText bool) []*Page {
	var out []*Page
	text := ""
	if keepText {
		text = p.Text
	}
	// Lazily, and once. 92% of pages are claimed by nothing at all, so extracting
	// a lead eagerly does the work for twelve pages in thirteen and throws it
	// away; and a page claimed as both a film and a person would otherwise scan
	// its own text twice for the same answer.
	var lead, plot string
	var got bool
	add := func(kind string, parsed any, ib map[string]string, links []PageLink) {
		if !got {
			lead, plot = wikitext.ExtractLeadAndPlot(p.Text)
			got = true
		}
		out = append(out, &Page{
			PageID: int(p.ID), Kind: kind, Title: p.Title, Wikitext: text,
			Infobox: ib, Lead: lead, Plot: plot, Parsed: parsed, Links: links,
		})
	}

	if m := buildFilm(p); m != nil {
		add(filmstock.KindMovie, m, m.Raw, linksOf(m.Raw,
			"director", "producer", "writer", "screenplay", "story", "starring",
			"music", "cinematography", "editing", "distributor", "studio",
			"production_companies", "based_on", "country", "language"))
	}
	if e := buildEvent(p); e != nil {
		add(filmstock.KindEvent, e, nil, nil)
	}
	if b := buildBiography(p); b != nil {
		// Every person, not the credited ones: which people matter is export's
		// question, and answering it during a streaming pass is what makes a
		// daily update unable to add anyone.
		add(filmstock.KindPerson, b, nil, nil)
	}
	if sc := buildSchedule(p); sc != nil {
		add(filmstock.KindSchedule, sc, nil, nil)
	}

	// Television, claimed exactly as parseTelevisionPage claims it. Anything
	// narrower loses pages silently: looking only for the two infoboxes finds no
	// episodes at all, because most episodes live on "List of X episodes"
	// articles that carry neither.
	//
	// These are stored as pages, not as parsed episode lists. The television
	// parser's output is a set of unexported messages that only mean anything
	// once the whole corpus has been collected, which is export's job — and the
	// wikitext is held, so re-parsing costs a scan rather than a dump.
	lower := strings.ToLower(p.Text)
	switch {
	case isSeasonArticle(p.Title, lower):
		ib := map[string]string{}
		if body, ok := wikitext.FindTemplate(p.Text, "Infobox television season"); ok {
			ib = wikitext.ParseInfobox(body)
		}
		add(kindSeason, nil, ib, linksOf(ib,
			"starring", "network", "episode_list", "prev_season", "next_season"))
	case hasSeriesInfobox(p.Text):
		body, _ := wikitext.FindTemplateExact(p.Text, "Infobox television")
		ib := wikitext.ParseInfobox(body)
		links := linksOf(ib, "creator", "starring", "composer", "director",
			"producer", "executive_producer", "writer", "editor",
			"cinematography", "presenter", "narrator", "network")
		links = append(links, titleLinksOf(ib, "list_episodes", "related")...)
		add(filmstock.KindTelevision, buildTelevisionSeries(p.Title, p.ID, ib), ib, links)
	case reListEpisodes.MatchString(p.Title):
		add(kindEpisodeList, nil, nil, nil)
	}
	return out
}

// isSeasonArticle and hasSeriesInfobox are the two claims parseTelevisionPage
// makes, named so that a test can assert import and extract agree about which
// pages are television. They diverged once already — 8,407 people got synthetic
// keys because a handler noted 3 of 11 fields — and the failure is silent.
func isSeasonArticle(title, lowerText string) bool {
	_, _, ok := articleSeason(title)
	return ok && (strings.Contains(lowerText, "{{infobox television season") ||
		strings.Contains(lowerText, "{{episode list"))
}

func hasSeriesInfobox(text string) bool {
	_, ok := wikitext.FindTemplateExact(text, "Infobox television")
	return ok
}

// linksOf pulls the stated link targets out of chosen infobox fields.
//
// The field is kept alongside the target because it is what tells export whether
// a link is a director, a network, or the episode list — the same target can be
// all three on different pages. And every target is kept: a film states a dozen
// starring links, so anything keyed by field alone would record one of them.
func linksOf(ib map[string]string, fields ...string) []PageLink {
	if ib == nil {
		return nil
	}
	var out []PageLink
	for _, f := range fields {
		v, ok := ib[f]
		if !ok || v == "" {
			continue
		}
		for _, l := range wikitext.SplitLinks(v) {
			if l.Wiki != "" {
				out = append(out, PageLink{Field: f, Target: l.Wiki})
			}
		}
	}
	return out
}

// titleLinksOf reads fields whose value IS a page title rather than a wikilink.
//
// A few infobox fields name an article without linking it: list_episodes is
// written "List of I Love Lucy episodes", not "[[List of I Love Lucy
// episodes]]". SplitLinks requires brackets, so the reference graph held 0 of
// 61,342 list_episodes edges — and that graph is what incremental export will
// use to decide what a change made stale. A missing edge there is a record that
// stays stale silently, which is this codebase's characteristic failure.
//
// Kept separate from linksOf rather than folded in as a fallback. Reading an
// unbracketed value as a page title is only correct where the field is defined
// to hold one: doing it for starring would turn every unlinked cast name into
// an edge pointing at an article that does not exist.
func titleLinksOf(ib map[string]string, fields ...string) []PageLink {
	if ib == nil {
		return nil
	}
	var out []PageLink
	for _, f := range fields {
		v, ok := ib[f]
		if !ok {
			continue
		}
		// Comments first: 57 series leave the template's own placeholder in
		// place — "<!-- name of list of episodes article goes here -->" — and it
		// would otherwise become a target.
		v = strings.TrimSpace(wikitext.ReComment.ReplaceAllString(v, ""))
		if v == "" {
			continue
		}
		if ls := wikitext.SplitLinks(v); len(ls) > 0 && ls[0].Wiki != "" {
			out = append(out, PageLink{Field: f, Target: ls[0].Wiki})
			continue
		}
		// CanonTitle drops a leading "#Episode List" — an anchor into the same
		// page, naming no article — and anything in the File:/Category:
		// namespaces.
		if t := wikitext.CanonTitle(v); t != "" {
			out = append(out, PageLink{Field: f, Target: t})
		}
	}
	return out
}

func defaultInterPath() string {
	if d, err := os.UserCacheDir(); err == nil {
		return d + "/filmstock/intermediate.db"
	}
	return "intermediate.db"
}

// importIncr applies one day of changes to the intermediate.
//
// This is the whole reason for the two-pass split. The daily path could not add
// a person: a day's dump carries the pages edited that day, and a new film's
// cast were not edited that day — their articles sit unchanged in a dump nobody
// is reading, and the last full pass discarded them because at that moment
// nothing had asked for them. 478 such people accumulated over 23 daily
// updates, none with a canonical identity.
//
// Here the day's pages land in a store that already holds every person in the
// encyclopaedia, so export resolves the new film's cast against articles it
// never had to see today.
//
// Pages are reconciled, not merely added: an adds-changes dump is the only
// place a page that stopped qualifying shows up, and it shows up as a page with
// its infobox removed rather than as a deletion.
func importIncr(path, interPath string, workers int, keepText bool) {
	in, err := OpenInter(interPath)
	if err != nil {
		fatal(err)
	}
	defer in.Close()
	if n, err := in.Pages(); err != nil {
		fatal(err)
	} else if n == 0 {
		fatal(fmt.Errorf("import: %s is empty — a day of changes applied to nothing "+
			"would produce a store holding only that day", interPath))
	}

	rc, err := dump.OpenBz2(path, workers)
	if err != nil {
		fatal(err)
	}
	defer rc.Close()

	start := time.Now()
	var pages, ns0, claimed, dropped int64
	err = dump.RunStream(rc, func(p dump.Page) {
		pages++
		if p.NS != 0 {
			return
		}
		ns0++
		claims := recognise(p, keepText)
		// Only pages we already hold, or newly claim, are worth a write. The
		// rest of a day's 100k+ edits are articles this project has never had
		// an opinion about.
		if len(claims) == 0 {
			had, err := in.Kinds(p.ID)
			if err != nil {
				fatal(err)
			}
			if len(had) == 0 {
				return
			}
			dropped += int64(len(had))
		}
		claimed += int64(len(claims))
		if err := in.Replace(p.ID, claims); err != nil {
			fatal(err)
		}
	})
	if err != nil {
		fatal(err)
	}
	if err := in.Flush(); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "applied %s in %.1fs\n", path, time.Since(start).Seconds())
	fmt.Fprintf(os.Stderr, "  pages=%d (ns0=%d) claims written=%d, stopped qualifying=%d\n",
		pages, ns0, claimed, dropped)
	fmt.Fprintln(os.Stderr, "  run `filmstock export` to rebuild the records")
}
