package build

import (
	"database/sql"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/szatmary/filmstock"
	"github.com/szatmary/filmstock/internal/dump"
	"github.com/szatmary/filmstock/internal/wikitext"
	_ "modernc.org/sqlite"
)

// `extract` turns a directory of dumps into the record hierarchy, in one command.
//
// The record hierarchy IS the repository; index.db and any derived index are
// derived from it and can be deleted and rebuilt without touching a dump. So the
// contract here is: delete OUTDIR, run this, get an identical repository back.
//
// The dependency graph lives in this file rather than in a shell script. Nothing
// the caller does can get the ordering wrong, and there are no "run X first"
// errors — if a prerequisite is missing it is built.
//
//	page_props.sql.gz ─┐
//	multistream index ─┴─▶ wiki_qid: page_id ↔ title ↔ Q-id   ─┐
//	                                                           ├─▶ enwiki dump
//	all.json.bz2 ────────▶ P179 / P4908 / labels             ─┘    (ONE pass)
//	                                                                    │
//	                                                movies/ television/ people/ text/
//
// Phases 1 and 2 are cached beside the DUMPS, not in the output. They are a pure
// function of the inputs, so wiping the output must not trigger a fresh ~50
// minute read of a 102 GB file.

type dumpSet struct {
	articles  string // enwiki-...-pages-articles-multistream.xml.bz2
	index     string // enwiki-...-multistream-index.txt.bz2
	pageProps string // enwiki-latest-page_props.sql.gz
	entities  string // wikidatawiki latest-all.json.bz2
	cache     string // resolver db, kept beside the dumps
}

// findDumps locates the inputs by suffix so the tool is not tied to one dump
// date, and reports everything missing at once rather than one at a time.
func findDumps(dir string) (*dumpSet, error) {
	names, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	d := &dumpSet{cache: filepath.Join(dir, "resolver.db")}
	for _, n := range names {
		p := filepath.Join(dir, n.Name())
		switch {
		case hasSuffix(n.Name(), "-pages-articles-multistream.xml.bz2"):
			d.articles = p
		case hasSuffix(n.Name(), "-multistream-index.txt.bz2"):
			d.index = p
		case hasSuffix(n.Name(), "page_props.sql.gz"):
			d.pageProps = p
		case hasSuffix(n.Name(), "all.json.bz2"):
			d.entities = p
		}
	}
	var missing []string
	for _, c := range []struct{ what, got string }{
		{"enwiki *-pages-articles-multistream.xml.bz2", d.articles},
		{"enwiki *-multistream-index.txt.bz2", d.index},
		{"enwiki *page_props.sql.gz", d.pageProps},
		{"wikidata *all.json.bz2", d.entities},
	} {
		if c.got == "" {
			missing = append(missing, c.what)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing dumps in %s:\n  %v", dir, missing)
	}
	return d, nil
}

func hasSuffix(s, suf string) bool {
	return len(s) >= len(suf) && s[len(s)-len(suf):] == suf
}

// tableExists reports whether the resolver cache already holds a built table,
// which is how phases 1 and 2 are skipped on a re-run.
func tableExists(dbPath, table string, col string) bool {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return false
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n == 0 {
		return false
	}
	if col != "" {
		var c int
		db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM pragma_table_info('%s') WHERE name=?`, table), col).Scan(&c)
		if c == 0 {
			return false
		}
	}
	var rows int
	db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM (SELECT 1 FROM %s LIMIT 1)`, table)).Scan(&rows)
	return rows > 0
}

func CExtract(args []string) {
	fs := flag.NewFlagSet("extract", flag.ExitOnError)
	dumpDir := fs.String("dumps", "dump", "directory holding the dumps (inputs; never written)")
	outDir := fs.String("out", "out", "output directory for the record hierarchy")
	workers := fs.Int("workers", 18, "parallel workers")
	force := fs.Bool("force", false, "rebuild the resolver cache even if present")
	cache := fs.String("cache", "", "resolver db (default <dumps>/resolver.db); build-time only, discardable")
	limit := fs.Int("limit", 0, "stop after this many films (0 = all); for smoke tests")
	skipText := fs.Bool("no-text", false, "skip the full-text corpus (faster; nothing is written to -text)")
	textDir := fs.String("text", "", "where the full-text corpus goes (default <out>/text). "+
		"Separate from -out so the records can live in a git repository while the "+
		"corpus, which nothing consumes yet, stays with the other derived artifacts")
	doIndex := fs.Bool("index", true, "also build the index when extraction finishes")
	dbPath := fs.String("db", "index.db", "the index to build when -index is set")
	fs.Parse(args)

	d, err := findDumps(*dumpDir)
	if err != nil {
		fatal(err)
	}
	if *cache != "" {
		d.cache = *cache
	}
	start := time.Now()

	// ---- Phase 1: Wikidata relations + multilingual names -------------------
	if *force || !tableExists(d.cache, "wd_part_of_series", "") {
		fmt.Fprintf(os.Stderr, "[1/4] wikidata: %s\n", d.entities)
		r, err := dump.OpenBz2(d.entities, *workers)
		if err != nil {
			fatal(err)
		}
		if err := buildWDEdges(r, d.cache, *workers, 2_000_000); err != nil {
			r.Close()
			fatal(err)
		}
		r.Close()
	} else {
		fmt.Fprintln(os.Stderr, "[1/4] wikidata: cached, skipping (-force to rebuild)")
	}

	// ---- Phase 2: page_id ↔ title ↔ Q-id ------------------------------------
	if *force || !tableExists(d.cache, "wiki_qid", "page_id") {
		fmt.Fprintf(os.Stderr, "[2/4] identity map: %s\n", d.pageProps)
		CBuildQidmap([]string{"-pageprops", d.pageProps, "-index", d.index, "-db", d.cache})
	} else {
		fmt.Fprintln(os.Stderr, "[2/4] identity map: cached, skipping (-force to rebuild)")
	}

	// ---- Phase 3: enwiki -> records ----------------------------------------
	fmt.Fprintf(os.Stderr, "[3/4] records: %s -> %s\n", d.articles, *outDir)
	// Default the corpus alongside the records, so a plain `extract -out DIR`
	// still behaves exactly as it always did.
	if *textDir == "" {
		*textDir = *outDir
	}
	if err := extractRecords(d, *outDir, *textDir, *workers, !*skipText, *limit); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "extract complete in %.1f min\n", time.Since(start).Minutes())

	// Index here by default: the records were just written, so they are still in
	// page cache and the 170k+ random reads that dominate a standalone index run
	// are nearly free. `filmstock index -records DIR` stays available on its own —
	// index.db is derived, so it must always be rebuildable without re-extracting.
	if *doIndex {
		fmt.Fprintln(os.Stderr, "[4/4] search index")
		CIndexRecords([]string{"-records", *outDir, "-db", *dbPath})
		fmt.Fprintf(os.Stderr, "extract+index complete in %.1f min\n", time.Since(start).Minutes())
	}
}

// extractRecords streams the article dump ONCE, feeding every page to both the
// film and the television extractors and writing the record hierarchy.
func extractRecords(d *dumpSet, outDir, textDir string, workers int, wantText bool, limit int) error {
	seasonOf, err := loadSeasonOf(d.cache)
	if err != nil {
		return err
	}

	for _, k := range []string{filmstock.KindMovie, filmstock.KindTelevision, filmstock.KindPerson} {
		if err := os.MkdirAll(filepath.Join(outDir, k), 0o755); err != nil {
			return err
		}
	}
	if wantText {
		if err := os.MkdirAll(filepath.Join(textDir, filmstock.KindText), 0o755); err != nil {
			return err
		}
	}

	// Encoding stays on the parser workers; only syscalls go to the pool. Depth is
	// generous because the array sustains only a few hundred IOPS and the parsers
	// must be free to run far ahead of it.
	rec := &recordWriter{out: outDir, textOut: textDir, wantText: wantText,
		store: newStoreWriter(outDir), people: map[string]*filmstock.PersonRecord{},
		bios: map[string]*filmstock.PersonBio{},
		pool: newWritePool(8, 16384)}

	coll := newTelevisionCollector()
	msgs := make(chan televisionMsg, 4096)
	collDone := make(chan struct{})
	go func() {
		for m := range msgs {
			coll.add(m)
		}
		close(collDone)
	}()

	var scanned int64
	handle := func(p dump.Page) {
		atomic.AddInt64(&scanned, 1)
		if p.NS != 0 {
			return
		}
		rec.handleFilm(p)
		rec.handleEvent(p)
		rec.handlePerson(p)
		for _, m := range parseTelevisionPage(p) {
			msgs <- m
		}
	}

	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		t0 := time.Now()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				n := atomic.LoadInt64(&scanned)
				fmt.Fprintf(os.Stderr, "\r  [%5.0fs] pages=%d (%.0f/s) films=%d",
					time.Since(t0).Seconds(), n, float64(n)/time.Since(t0).Seconds(),
					atomic.LoadInt64(&rec.films))
			}
		}
	}()

	var shouldStop func() bool
	if limit > 0 {
		shouldStop = func() bool { return atomic.LoadInt64(&rec.films) >= int64(limit) }
	}
	if err := dump.RunMultistream(d.articles, d.index, workers, handle, shouldStop); err != nil {
		fatal(err)
	}
	close(msgs)
	<-collDone
	close(stop)

	listOwner, err := buildListOwner(d.cache, coll)
	if err != nil {
		return err
	}
	series, st := coll.finish(seasonOf, listOwner)
	for _, s := range series {
		rec.handleSeries(s)
	}
	withQID, withBio, nPeople := rec.flushPeople(d.cache)
	if err := rec.pool.close(); err != nil {
		rec.fail(err)
	}

	fmt.Fprintf(os.Stderr, "  events=%d (award ceremonies + film festivals)\n", rec.events)
	fmt.Fprintf(os.Stderr, "\n  pages=%d films=%d series=%d episodes=%d\n",
		scanned, rec.films, st.Series, st.Episodes)
	pct := 0
	if nPeople > 0 {
		pct = 100 * withQID / nPeople
	}
	bioPct := 0
	if nPeople > 0 {
		bioPct = 100 * withBio / nPeople
	}
	fmt.Fprintf(os.Stderr, "  people=%d  with Q-id=%d (%d%%)  link-target only=%d\n",
		nPeople, withQID, pct, nPeople-withQID)
	fmt.Fprintf(os.Stderr, "  biographies=%d of %d people (%d%%)  from %d person articles in the dump\n",
		withBio, nPeople, bioPct, len(rec.bios))
	fmt.Fprintf(os.Stderr, "  season->series: resolved=%d unresolved=%d\n", st.Resolved, st.Unresolved)
	if st.Unresolved > 0 {
		fmt.Fprintf(os.Stderr, "  unattached episode sources (sample): %v\n", st.OrphanNames)
	}
	return rec.err
}

type recordWriter struct {
	out      string
	textOut  string // corpus root; may sit outside -out entirely
	wantText bool
	films    int64
	events   int64
	pool     *writePool

	store  *storeWriter
	mu     sync.Mutex
	people map[string]*filmstock.PersonRecord
	// bios is keyed by article title. A biography is encountered at an arbitrary
	// point in the stream — long before or long after the film that credits its
	// subject — so it cannot be attached on sight. It is held here and joined to
	// the discovered people once the pass is over.
	bios map[string]*filmstock.PersonBio
	err  error
}

func (w *recordWriter) fail(e error) {
	w.mu.Lock()
	if w.err == nil {
		w.err = e
	}
	w.mu.Unlock()
}

// notePeople records every credit that carries a stated identity.
func (w *recordWriter) notePeople(groups ...[]filmstock.Person) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, g := range groups {
		for _, p := range g {
			if p.Wiki == "" {
				continue // no link, no identity, not an entity
			}
			if _, ok := w.people[p.Wiki]; !ok {
				w.people[p.Wiki] = &filmstock.PersonRecord{Wiki: p.Wiki, Name: p.Name}
			}
		}
	}
}

// handlePerson keeps the biography of every person-shaped article in the dump.
// Most will never be credited on anything and are dropped at the end; which ones
// matter is not knowable until the whole stream has been read.
func (w *recordWriter) handlePerson(p dump.Page) {
	b := buildBiography(p)
	if b == nil {
		return
	}
	w.mu.Lock()
	w.bios[wikitext.CanonTitle(p.Title)] = b
	w.mu.Unlock()
}

func (w *recordWriter) handleFilm(p dump.Page) {
	m := buildFilm(p)
	if m == nil {
		return
	}
	w.store.put(filmstock.KindMovie, int64(p.ID), m)
	atomic.AddInt64(&w.films, 1)
	w.notePeople(m.Director, m.Producer, m.Writer, m.Starring, m.Music, m.Cinematography, m.Editing)
	if w.wantText {
		if td, err := encodeRecordText(wikitext.FullPlainText(p.Text)); err != nil {
			w.fail(err)
		} else {
			w.pool.put(filmstock.RecordPath(w.textOut, filmstock.KindText, int64(p.ID), ".txt.gz"), td)
		}
	}
}

// handleEvent writes award ceremonies and film festivals. They are checked on
// every page alongside handleFilm rather than as an else-branch: the two are
// mutually exclusive by construction now that both match their template
// exactly, and an else-branch would silently hide it if that ever stopped being
// true.
func (w *recordWriter) handleEvent(p dump.Page) {
	e := buildEvent(p)
	if e == nil {
		return
	}
	w.store.put(filmstock.KindEvent, int64(p.ID), e)
	atomic.AddInt64(&w.events, 1)
	w.notePeople(e.Hosts)
}

func (w *recordWriter) handleSeries(s *filmstock.TelevisionSeries) {
	w.store.put(filmstock.KindTelevision, int64(s.PageID), s)
	w.notePeople(s.Creator, s.Starring, s.Composer)
	for _, se := range s.Seasons {
		for _, e := range se.Episodes {
			w.notePeople(e.DirectedBy, e.WrittenBy)
		}
	}
}

// flushPeople resolves each person's Q-id from the link target and writes one
// record per identity.
//
// Resolution goes ONLY through the link target. Looking a bare display name up
// as an article title would attach every unlinked "John Smith" to whoever holds
// that title — an invented identity, and silently wrong.
func (w *recordWriter) flushPeople(cachePath string) (withQID, withBio, total int) {
	db, err := sql.Open("sqlite", cachePath)
	if err != nil {
		w.fail(err)
		return
	}
	defer db.Close()
	stmt, err := db.Prepare(`SELECT qid FROM wiki_qid WHERE title = ?`)
	if err != nil {
		w.fail(err)
		return
	}
	defer stmt.Close()

	for _, p := range w.people {
		var q int64
		if stmt.QueryRow(p.Wiki).Scan(&q) == nil && q > 0 {
			p.QID = q
			withQID++
		}
		// Join the biography read from this person's own article. A miss is
		// ordinary and is counted, not guessed at: the credit may link to a
		// redlink, a disambiguation page, or something that is not a person.
		if b, ok := w.bios[wikitext.CanonTitle(p.Wiki)]; ok {
			p.PersonBio = b
			withBio++
		}
		p.WikiURL = "https://en.wikipedia.org/wiki/" +
			strings.ReplaceAll(url.PathEscape(p.Wiki), "%20", "_")
		id := p.QID
		if id == 0 {
			// No Q-id yet: the link target is still a stated identity, so keep
			// the person. Negative ids keep the two path spaces disjoint; the
			// record itself carries the real key (Wiki), and a later extract
			// upgrades it to a Q-id for free.
			id = -int64(filmstock.PersonRecordPathID(p.Wiki))
		}
		w.store.put(filmstock.KindPerson, id, p)
		total++
	}
	return
}

// buildListOwner inverts each series' stated list_episodes link into
// "episode-list page_id -> series page_id".
//
// A "List of X episodes" article is not a season, so it carries no P179 and was
// left unattached — 183k episodes' worth. But the series article names its list
// explicitly in the infobox, and that pointer is as stated as any other wikilink.
// Titles are resolved to page_ids through wiki_qid so the join itself never
// compares strings.
func buildListOwner(cachePath string, coll *televisionCollector) (map[int]int, error) {
	db, err := sql.Open("sqlite", cachePath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	stmt, err := db.Prepare(`SELECT page_id FROM wiki_qid WHERE title = ?`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	out := map[int]int{}
	for seriesID, s := range coll.series {
		t := wikitext.CanonTitle(s.ListEpisodes)
		if t == "" {
			continue
		}
		var pid int
		if stmt.QueryRow(t).Scan(&pid) == nil && pid != 0 {
			out[pid] = seriesID
		}
	}
	fmt.Fprintf(os.Stderr, "episode-list links resolved: %d\n", len(out))
	return out, nil
}
