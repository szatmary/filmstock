package build

import (
	"compress/gzip"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/szatmary/filmstock"
	"github.com/szatmary/filmstock/internal/dump"
	"github.com/szatmary/filmstock/internal/wikitext"
)

// televisionMsg is a unit of television data emitted by parseTelevisionPage: either a series record or a
// batch of episodes tagged with the page_id of the article they came from.
//
// Episodes are deliberately NOT tagged with a series title. A title with its
// disambiguator stripped cannot tell "Four on the Floor (Canadian TV series)"
// from "(American TV program)", so the season->series edge is resolved later
// from Wikidata's stated P179 ("part of the series") relation rather than being
// derived from strings.
type televisionMsg struct {
	series *filmstock.TelevisionSeries

	srcID      int    // page_id of the article these episodes came from
	srcTitle   string // diagnostics only — never a key
	seriesID   int    // series page_id when the source article IS the series
	season     int
	eps        []*filmstock.Episode
	seasonMeta *filmstock.Season
	auth       bool // episodes from a dedicated season article
}

// parseTelevisionPage extracts series and/or episode data from one article.
func parseTelevisionPage(p dump.Page) []televisionMsg {
	text := p.Text
	lower := strings.ToLower(text)
	var msgs []televisionMsg

	// Season article: episodes all belong to the season named in the title.
	if _, season, ok := articleSeason(p.Title); ok &&
		(strings.Contains(lower, "{{infobox television season") || strings.Contains(lower, "{{episode list")) {
		var meta *filmstock.Season
		if body, ok := wikitext.FindTemplate(text, "Infobox television season"); ok {
			ib := wikitext.ParseInfobox(body)
			// PageID: a season article is a real page, so a season is
			// addressable like everything else rather than being reachable only
			// as an index into its series.
			meta = &filmstock.Season{Season: season, PageID: p.ID}
			if d := parseReleaseDates(ib["first_aired"]); len(d) > 0 {
				meta.FirstAired = d[0]
			}
			if d := parseReleaseDates(ib["last_aired"]); len(d) > 0 {
				meta.LastAired = d[0]
			}
			// The season's own cast, which the series-level list cannot express:
			// one flat Starring for a fifteen-season run asserts that everyone
			// who ever appeared was in it throughout.
			meta.Starring = wikitext.SplitPeople(ib["starring"])
			meta.Network = wikitext.CleanText(ib["network"])
			meta.Image = strings.TrimSpace(ib["image"])
			if n := firstInt(ib["num_episodes"]); n > 0 {
				meta.NumEpisodes = n
			}
		}
		var eps []*filmstock.Episode
		for _, body := range wikitext.FindAllTemplates(text, "Episode list") {
			eps = append(eps, parseEpisodeRows(wikitext.ParseInfobox(body))...)
		}
		if len(eps) > 0 || meta != nil {
			// The season article names its season but never its series, so the
			// owner is left blank here for Wikidata to supply.
			msgs = append(msgs, televisionMsg{srcID: p.ID, srcTitle: p.Title, season: season,
				eps: eps, seasonMeta: meta, auth: true})
		}
		return msgs
	}

	// Series article — exact match so "…episode"/"…season" infoboxes don't leak in.
	if body, ok := wikitext.FindTemplateExact(text, "Infobox television"); ok {
		s := buildTelevisionSeries(p.Title, p.ID, wikitext.ParseInfobox(body))
		s.Overview, s.Plot = wikitext.ExtractLeadAndPlot(text)
		msgs = append(msgs, televisionMsg{series: s})
		// Episodes listed inside the series article belong to that series by
		// construction — no resolution needed.
		msgs = append(msgs, episodesByHeading(text, p.ID, p.Title, p.ID)...)
		msgs = append(msgs, overviewMsgs(text, p.ID, p.Title, p.ID)...)
		return msgs
	}

	// "List of X episodes" article. Its owner is not stated here either.
	if reListEpisodes.MatchString(p.Title) {
		msgs = append(msgs, episodesByHeading(text, p.ID, p.Title, 0)...)
		msgs = append(msgs, overviewMsgs(text, p.ID, p.Title, 0)...)
	}
	return msgs
}

// overviewMsgs turns a page's {{Series overview}} into per-season metadata.
//
// It is the only place the corpus states a season's Nielsen standing, and it
// covers seasons that have no article of their own — which is most seasons of
// most shows, and so most of what this adds.
func overviewMsgs(text string, srcID int, srcTitle string, seriesID int) []televisionMsg {
	var msgs []televisionMsg
	for _, sea := range parseSeriesOverview(text) {
		if sea.Season <= 0 {
			continue
		}
		msgs = append(msgs, televisionMsg{srcID: srcID, srcTitle: srcTitle,
			seriesID: seriesID, season: sea.Season, seasonMeta: sea})
	}
	return msgs
}

// episodesByHeading groups an article's inline episodes into per-season messages,
// tagged with the source article's page_id (and its series, when self-evident).
func episodesByHeading(text string, srcID int, srcTitle string, seriesID int) []televisionMsg {
	bySeason := map[int][]*filmstock.Episode{}
	for _, te := range extractEpisodesByHeading(text) {
		s := te.season
		if s == 0 {
			s = 1
		}
		bySeason[s] = append(bySeason[s], te.ep)
	}
	var msgs []televisionMsg
	for s, list := range bySeason {
		msgs = append(msgs, televisionMsg{srcID: srcID, srcTitle: srcTitle, seriesID: seriesID,
			season: s, eps: list})
	}
	return msgs
}

// televisionCollector merges television messages into series with nested seasons/episodes.
// Episodes from season articles (auth) take priority over inline list-article
// episodes, which are only used for seasons with no authoritative data.
type televisionCollector struct {
	// Identity is the page_id, never the title. Keying by a
	// disambiguator-stripped title silently dropped every show that collided
	// with another (the BBC's House of Cards, PBS Frontline, all non-US Big
	// Brothers), because the loser was simply overwritten.
	series map[int]*filmstock.TelevisionSeries

	// Episodes are staged by SOURCE article page_id until the owning series is
	// known. auth = from a dedicated season article, which outranks inline
	// episodes scraped from a "List of X episodes" page.
	auth   map[int]map[int][]*filmstock.Episode
	inline map[int]map[int][]*filmstock.Episode
	smeta  map[int]map[int]*filmstock.Season

	srcTitle map[int]string // page_id -> title, for reporting unresolved sources
	srcOwner map[int]int    // page_id -> series page_id, when the source states it
}

func newTelevisionCollector() *televisionCollector {
	return &televisionCollector{
		series:   map[int]*filmstock.TelevisionSeries{},
		auth:     map[int]map[int][]*filmstock.Episode{},
		inline:   map[int]map[int][]*filmstock.Episode{},
		smeta:    map[int]map[int]*filmstock.Season{},
		srcTitle: map[int]string{},
		srcOwner: map[int]int{},
	}
}

func addEps(m map[int]map[int][]*filmstock.Episode, key int, season int, eps []*filmstock.Episode) {
	sm := m[key]
	if sm == nil {
		sm = map[int][]*filmstock.Episode{}
		m[key] = sm
	}
	sm[season] = append(sm[season], eps...)
}

func (c *televisionCollector) add(m televisionMsg) {
	if m.series != nil {
		id := m.series.PageID
		if old, ok := c.series[id]; !ok || len(m.series.Raw) > len(old.Raw) {
			c.series[id] = m.series
		}
	}
	if m.srcID != 0 && (len(m.eps) > 0 || m.seasonMeta != nil) {
		if m.auth {
			addEps(c.auth, m.srcID, m.season, m.eps)
		} else {
			addEps(c.inline, m.srcID, m.season, m.eps)
		}
		if m.seasonMeta != nil {
			mm := c.smeta[m.srcID]
			if mm == nil {
				mm = map[int]*filmstock.Season{}
				c.smeta[m.srcID] = mm
			}
			// Merge: one page can state a season twice — an episode-list
			// article carries both a {{Series overview}} row and, further down,
			// the season's own heading. Overwriting keeps whichever arrived
			// last and drops the other's fields.
			if old, ok := mm[m.season]; ok {
				mergeSeason(old, m.seasonMeta)
			} else {
				mm[m.season] = m.seasonMeta
			}
		}
		c.srcTitle[m.srcID] = m.srcTitle
		if m.seriesID != 0 {
			c.srcOwner[m.srcID] = m.seriesID
		}
	}
}

// televisionStats reports how the season->series resolution went, so a drop in coverage
// is visible in the run output instead of silently shrinking the data.
type televisionStats struct {
	Series      int
	Episodes    int
	Resolved    int      // episode sources attached to a series
	Unresolved  int      // episode sources with no stated owner
	OrphanNames []string // a sample of unresolved source titles
}

// finish assembles final series records, attaching sorted, de-duplicated seasons.
//
// seasonOf maps an episode-source article page_id to its series page_id, built
// from Wikidata's stated "part of the series" (P179) relation. A source with no stated
// owner is left UNATTACHED and counted — never guessed onto a same-titled show,
// which is exactly how the two Hawaii Five-Os and fifty Big Brothers would merge.
// listOwner maps a "List of X episodes" article page_id to its series page_id,
// built by inverting the series' own list_episodes infobox link.
func (c *televisionCollector) finish(seasonOf, listOwner map[int]int) ([]*filmstock.TelevisionSeries, televisionStats) {
	var st televisionStats

	// Map each episode source to its owning series.
	sources := map[int]bool{}
	for id := range c.auth {
		sources[id] = true
	}
	for id := range c.inline {
		sources[id] = true
	}
	for id := range c.smeta {
		sources[id] = true
	}
	bySeries := map[int][]int{}
	for src := range sources {
		owner, ok := c.srcOwner[src]
		if !ok {
			owner, ok = seasonOf[src]
		}
		if !ok {
			// The series states where its episode list lives; inverting that
			// link is as authoritative as P179, just authored on enwiki.
			owner, ok = listOwner[src]
		}
		if ok {
			if _, known := c.series[owner]; known {
				bySeries[owner] = append(bySeries[owner], src)
				st.Resolved++
				continue
			}
		}
		st.Unresolved++
		if len(st.OrphanNames) < 25 {
			st.OrphanNames = append(st.OrphanNames, c.srcTitle[src])
		}
	}

	var out []*filmstock.TelevisionSeries
	for id, s := range c.series {
		srcs := bySeries[id]
		// In page_id order. srcs is built by ranging over a map, so without this
		// the sources are merged in a different order on every run — and where a
		// season is described by more than one of them, whichever came first won.
		// That is 164 seasons changing between two runs over identical input,
		// mostly num_episodes disagreeing by one.
		sort.Ints(srcs)
		// Union of season numbers from authoritative and inline sources.
		seasons := map[int][]*filmstock.Episode{}
		for _, src := range srcs {
			for snum, list := range c.inline[src] {
				seasons[snum] = append(seasons[snum], list...)
			}
		}
		authSeasons := map[int][]*filmstock.Episode{}
		for _, src := range srcs {
			for snum, list := range c.auth[src] {
				authSeasons[snum] = append(authSeasons[snum], list...)
			}
		}
		for snum, list := range authSeasons {
			seasons[snum] = list // authoritative overrides inline for this season
		}
		// A season stated only by {{Series overview}} is still a season. Before
		// this, seasons were enumerated from episode lists alone, so a show
		// whose overview gives twenty seasons of counts, dates and ratings but
		// whose episode rows would not parse produced no seasons at all.
		for _, src := range srcs {
			for snum := range c.smeta[src] {
				if _, ok := seasons[snum]; !ok {
					seasons[snum] = nil
				}
			}
		}
		for snum, list := range seasons {
			seen := map[string]bool{}
			var ded []*filmstock.Episode
			for _, e := range list {
				// Number AND title, not number alone.
				//
				// The number alone is enough to recognise the same episode
				// arriving from two sources, and it was what this used. But a
				// SERIAL is several distinct episodes that share one number:
				// classic Doctor Who's "Robot" is four broadcasts numbered as
				// one story, so three of every four were discarded here even
				// after being parsed correctly. 615 broadcast parts across 142
				// serials.
				//
				// The looser key can only fail the other way — the same episode
				// listed twice under different titles surviving twice — and
				// that is bounded, because an authoritative season article
				// replaces the inline list for its season rather than merging
				// with it, so within a season the episodes almost always come
				// from one source.
				key := "n|" + strconv.Itoa(e.NumberInSeason) + "|" + e.Title
				if seen[key] {
					continue
				}
				seen[key] = true
				ded = append(ded, e)
			}
			sort.SliceStable(ded, func(i, j int) bool {
				if ded[i].NumberInSeason != ded[j].NumberInSeason {
					return ded[i].NumberInSeason < ded[j].NumberInSeason
				}
				return ded[i].NumberOverall < ded[j].NumberOverall
			})
			sea := &filmstock.Season{Season: snum, Episodes: ded, NumEpisodes: len(ded)}
			// Every source, not the first: the season's own article knows its
			// cast, network and page_id, while the series overview knows its
			// Nielsen standing, and neither knows the other's. Stopping at the
			// first source that mentions the season keeps whichever happened to
			// be visited first and discards the rest.
			for _, src := range srcs {
				if meta, ok := c.smeta[src][snum]; ok {
					mergeSeason(sea, meta)
				}
			}
			// The stated count only when no episodes parsed. Where rows did
			// parse, NumEpisodes must agree with Episodes or the record
			// contradicts itself.
			if len(ded) > 0 {
				sea.NumEpisodes = len(ded)
			}
			s.Seasons = append(s.Seasons, sea)
			st.Episodes += len(ded)
		}
		sort.SliceStable(s.Seasons, func(i, j int) bool { return s.Seasons[i].Season < s.Seasons[j].Season })
		out = append(out, s)
	}
	st.Series = len(out)
	return out, st
}

// mergeSeason fills dst's empty fields from src, and never overwrites.
//
// Season data arrives from two places that know different things — the season
// article for cast, network, image and its own page_id; {{Series overview}} for
// rank, rating and viewers — so combining them is addition, not replacement.
func mergeSeason(dst, src *filmstock.Season) {
	if src == nil || dst == nil {
		return
	}
	if dst.PageID == 0 {
		dst.PageID = src.PageID
	}
	if dst.FirstAired == "" {
		dst.FirstAired = src.FirstAired
	}
	if dst.LastAired == "" {
		dst.LastAired = src.LastAired
	}
	if dst.Network == "" {
		dst.Network = src.Network
	}
	if dst.Image == "" {
		dst.Image = src.Image
	}
	if len(dst.Starring) == 0 {
		dst.Starring = src.Starring
	}
	if dst.NumEpisodes == 0 {
		dst.NumEpisodes = src.NumEpisodes
	}
	if dst.Rank == 0 {
		dst.Rank = src.Rank
	}
	if dst.Rating == 0 {
		dst.Rating = src.Rating
	}
	if dst.Viewers == 0 {
		dst.Viewers = src.Viewers
	}
}

var reTelevisionSlug = reSlug

// writeTelevisionSeries writes one gzip-compressed series JSON, sharded like movies.
func writeTelevisionSeries(outDir string, s *filmstock.TelevisionSeries) error {
	sum := md5.Sum([]byte(s.Title))
	dir := filepath.Join(outDir, hex.EncodeToString(sum[:1]))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	slug := reTelevisionSlug.ReplaceAllString(s.Title, "_")
	if len(slug) > 80 {
		slug = slug[:80]
	}
	fname := fmt.Sprintf("%s-%d.json.gz", slug, s.PageID)
	f, err := os.Create(filepath.Join(dir, fname))
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	enc := json.NewEncoder(gz)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

// CTelevisionTest assembles television from local wikitext files to validate the pipeline.
// Usage: filmstock television-test title=file [title=file ...]
func CTelevisionTest(args []string) {
	coll := newTelevisionCollector()
	var pid int
	for _, a := range args {
		i := strings.Index(a, "=")
		if i < 0 {
			continue
		}
		title, path := a[:i], a[i+1:]
		data, err := os.ReadFile(path)
		if err != nil {
			fatal(err)
		}
		pid++
		// mimic xml.Decoder entity unescaping that the streaming parser gets
		p := dump.Page{Title: title, NS: 0, ID: pid, Text: html.UnescapeString(string(data))}
		for _, m := range parseTelevisionPage(p) {
			coll.add(m)
		}
	}
	series, _ := coll.finish(nil, nil)
	for _, s := range series {
		s.Raw = nil
		out, _ := json.MarshalIndent(s, "", "  ")
		fmt.Println(string(out))
	}
}

// CTelevisionDebug parses a single wikitext file and prints what the television parser sees.
// Usage: filmstock television-debug <wikitext-file> [title]
func CTelevisionDebug(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: filmstock television-debug <wikitext-file> [title]")
		os.Exit(2)
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		fatal(err)
	}
	text := string(data)
	title := "Sample"
	if len(args) > 1 {
		title = args[1]
	}

	if body, ok := wikitext.FindTemplate(text, "Infobox television season"); ok {
		ib := wikitext.ParseInfobox(body)
		fmt.Printf("SEASON ARTICLE: season_number=%s first_aired=%s\n",
			ib["season_number"], firstNonEmpty(ib["first_aired"]))
	} else if body, ok := wikitext.FindTemplate(text, "Infobox television"); ok {
		s := buildTelevisionSeries(title, 0, wikitext.ParseInfobox(body))
		s.Raw = nil
		out, _ := json.MarshalIndent(s, "", "  ")
		fmt.Println("SERIES:")
		fmt.Println(string(out))
	}

	if series, season, ok := articleSeason(title); ok {
		fmt.Printf("TITLE parses as season: series=%q season=%d\n", series, season)
	}

	eps := extractEpisodesByHeading(text)
	fmt.Printf("\nEPISODES found: %d\n", len(eps))
	for i, te := range eps {
		if i >= 6 {
			fmt.Println("  ...")
			break
		}
		fmt.Printf("  [s%d] #%d.%d %q air=%s dir=%v writ=%v\n",
			te.season, te.ep.NumberOverall, te.ep.NumberInSeason, te.ep.Title,
			te.ep.AirDate, te.ep.DirectedBy, te.ep.WrittenBy)
	}
}
