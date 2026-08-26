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
	if err := extractRecords(d, dumpSource(d, *workers), *outDir, *textDir, *workers, !*skipText, *limit); err != nil {
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

// A pageSource feeds pages to the record builders.
//
// There are two, and they must be interchangeable: the article dump, and the
// intermediate replaying the wikitext it stored. Interchangeable is the whole
// claim of the two-pass split — a record built from the intermediate has to be
// the record extract would have built, or the cheap path quietly produces
// different data from the expensive one.
type pageSource struct {
	name  string // what to call it in the log
	total int64  // denominator for progress, 0 if unknown
	run   func(handle func(dump.Page), shouldStop func() bool, prog *atomic.Int64) error
}

// dumpSource reads the article dump, one bz2 sub-stream per worker.
func dumpSource(d *dumpSet, workers int) pageSource {
	total, _ := dump.DumpSize(d.articles)
	return pageSource{name: d.articles, total: total,
		run: func(handle func(dump.Page), stop func() bool, prog *atomic.Int64) error {
			return dump.RunMultistreamProgress(d.articles, d.index, workers, handle, stop, prog)
		}}
}

// extractRecords feeds every page to both the film and the television
// extractors and writes the record hierarchy.
func extractRecords(d *dumpSet, src pageSource, outDir, textDir string, workers int, wantText bool, limit int) error {
	seasonOf, err := loadSeasonOf(d.cache)
	if err != nil {
		return err
	}

	// A store carries the dictionary it was written with. Seed a new one from
	// what this build embeds; an existing store keeps its own, because the
	// records already in it can only be read with that.
	if err := filmstock.WriteDictionaries(outDir); err != nil {
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
	sw := newStoreWriter(outDir)
	// A complete run derives every record, so it can also say which are gone.
	// A -limit run has not reached most of them and must not sweep.
	if limit == 0 {
		sw.wrote = map[string]map[string]bool{}
	}
	rec := &recordWriter{out: outDir, textOut: textDir, wantText: wantText,
		store: sw, people: map[string]*filmstock.PersonRecord{},
		bios:      map[string]*filmstock.PersonBio{},
		bioPage:   map[string]int{},
		workTitle: map[string]bool{},
		pool:      newWritePool(8, 16384)}

	coll := newTelevisionCollector()
	msgs := make(chan televisionMsg, 4096)
	collDone := make(chan struct{})
	go func() {
		for m := range msgs {
			coll.add(m)
		}
		close(collDone)
	}()

	var progress atomic.Int64
	total := src.total

	var scanned int64
	handle := func(p dump.Page) {
		atomic.AddInt64(&scanned, 1)
		if p.NS != 0 {
			return
		}
		rec.handleFilm(p)
		rec.handleEvent(p)
		rec.handlePerson(p)
		rec.handleSchedule(p)
		for _, m := range parseTelevisionPage(p) {
			if m.series != nil {
				// The ARTICLE title, not the record's display title: the
				// display one has had its disambiguator stripped, and a credit
				// links to the article.
				rec.noteWork(p.Title)
			}
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
				el := time.Since(t0).Seconds()
				pct, eta := 0.0, ""
				if done := progress.Load(); total > 0 && done > 0 {
					pct = 100 * float64(done) / float64(total)
					if rate := float64(done) / el; rate > 0 {
						eta = fmt.Sprintf("  eta %s",
							(time.Duration(float64(total-done)/rate) * time.Second).Round(time.Minute))
					}
				}
				// Written with \r for a terminal but also newline-terminated at
				// intervals, because a progress line that only ever rewrites
				// itself disappears entirely when stderr is piped — which is how
				// an 85-minute run once produced a 181-byte log.
				line := fmt.Sprintf("  [%5.0fs] %5.1f%%  pages=%d (%.0f/s) films=%d%s",
					el, pct, n, float64(n)/el, atomic.LoadInt64(&rec.films), eta)
				if isTerminal(os.Stderr) {
					fmt.Fprintf(os.Stderr, "\r%s", line)
				} else {
					fmt.Fprintln(os.Stderr, line)
				}
			}
		}
	}()

	var shouldStop func() bool
	if limit > 0 {
		shouldStop = func() bool { return atomic.LoadInt64(&rec.films) >= int64(limit) }
	}
	if err := src.run(handle, shouldStop, &progress); err != nil {
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
	withQID, withBio, nPeople, noIdentity := rec.flushPeople(d.cache)
	nSched, nSlots, nLinked := rec.flushSchedules(d.cache)
	// After everything is written, and only then: what is left is stale.
	if removed := rec.store.sweep(); removed > 0 {
		fmt.Fprintf(os.Stderr, "  swept %d records the encyclopaedia no longer supports\n", removed)
	}
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
	fmt.Fprintf(os.Stderr, "  people=%d  all keyed by page_id  with Q-id=%d (%d%%)\n",
		nPeople, withQID, pct)
	fmt.Fprintf(os.Stderr, "  %d credits had no article and so no record; "+
		"they remain credits on the works that state them\n", noIdentity)
	fmt.Fprintf(os.Stderr, "  biographies=%d of %d people (%d%%)  from %d person articles in the dump\n",
		withBio, nPeople, bioPct, len(rec.bios))
	if nSched > 0 {
		pct := 0
		if nSlots > 0 {
			pct = 100 * nLinked / nSlots
		}
		fmt.Fprintf(os.Stderr, "  schedules=%d grids, %d slots, %d linked to a series (%d%%)\n",
			nSched, nSlots, nLinked, pct)
	}
	// What the store actually took. An ingest that leaves most records alone is
	// the whole point of the format; saying so makes a regression visible.
	if un, up, ins := rec.store.Counts(); un+up+ins > 0 {
		fmt.Fprintf(os.Stderr, "  store: %d unchanged, %d updated, %d inserted\n", un, up, ins)
	}
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
	// bioPage is the page_id of each biography article, keyed the same way as
	// bios. It is the person's identity, and it comes from the dump rather than
	// from wiki_qid so that a person whose article carries no Wikidata item still
	// gets a canonical id.
	bioPage map[string]int

	// workTitle holds the article title of every film, series and event, so a
	// credit that links to one can be recognised as not being a person. Page
	// titles are unique on Wikipedia, so a title that names a film cannot also
	// name a person.
	workTitle map[string]bool

	// schedules are held until the pass is over: their show ids resolve through
	// the same cache the people pass uses, and doing it per cell would be a
	// query for every slot in every season.
	schedules []*filmstock.Schedule
	err       error
}

func (w *recordWriter) fail(e error) {
	w.mu.Lock()
	if w.err == nil {
		w.err = e
	}
	w.mu.Unlock()
}

// noteWork records that an article title names a film, series or event.
func (w *recordWriter) noteWork(title string) {
	t := wikitext.CanonTitle(title)
	if t == "" {
		return
	}
	w.mu.Lock()
	w.workTitle[t] = true
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
	title := wikitext.CanonTitle(p.Title)
	w.bios[title] = b
	// The page_id of the person's own article, which is their identity. Taken
	// from the dump rather than looked up: this covers people whose article
	// carries no Wikidata item and who are therefore absent from wiki_qid.
	w.bioPage[title] = int(p.ID)
	w.mu.Unlock()
}

// handleFilmRecord is the half of handleFilm that stores a parsed film and notes
// its people, split out so a test can drive the real code rather than restate it.
func (w *recordWriter) handleFilmRecord(m *filmstock.Movie, pageID int64) {
	w.store.put(filmstock.KindMovie, pageID, m)
	atomic.AddInt64(&w.films, 1)
	w.notePeople(m.Director, m.Producer, m.Writer, m.Starring, m.Music,
		m.Cinematography, m.Editing, m.Narrator)
}

func (w *recordWriter) handleFilm(p dump.Page) {
	m := buildFilm(p)
	if m != nil {
		w.noteWork(p.Title)
	}
	if m == nil {
		return
	}
	w.handleFilmRecord(m, int64(p.ID))
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
// handleEventRecord is the half of handleEvent that stores a parsed event and
// notes its people, split out so a test can drive the real code rather than
// restate it.
func (w *recordWriter) handleEventRecord(e *filmstock.Event) {
	w.store.put(filmstock.KindEvent, int64(e.PageID), e)
	w.notePeople(e.Hosts)
}

func (w *recordWriter) handleEvent(p dump.Page) {
	e := buildEvent(p)
	if e == nil {
		return
	}
	w.noteWork(p.Title)
	w.handleEventRecord(e)
	atomic.AddInt64(&w.events, 1)
}

// handleSchedule keeps a network television schedule grid.
//
// Show ids are not resolved here: a season repeats the same programme in dozens
// of slots and the resolver cache is a per-lookup query, so it is done once for
// the whole run after the pass, alongside the other cache work.
func (w *recordWriter) handleSchedule(p dump.Page) {
	sc := buildSchedule(p)
	if sc == nil {
		return
	}
	sc.WikiURL = "https://en.wikipedia.org/wiki/" +
		strings.ReplaceAll(url.PathEscape(p.Title), "%20", "_")
	w.mu.Lock()
	w.schedules = append(w.schedules, sc)
	w.mu.Unlock()
}

func (w *recordWriter) handleSeries(s *filmstock.TelevisionSeries) {
	w.store.put(filmstock.KindTelevision, int64(s.PageID), s)
	// EVERY person-bearing field has to be noted here. Noting only some of them
	// does not drop those people from the database — the index still builds
	// credits from the series record — it drops them from Q-id resolution, so
	// they end up as index rows with a null qid and no record in the store at
	// all. That is how 8,407 people came to be keyed by a hash of their article
	// path while their Q-id sat in the resolver cache unread: this call listed
	// Creator, Starring and Composer, and the television enrichment had since
	// added eight more fields. Their credits were 6,775 Presenter, 2,672
	// Executive Producer, 1,688 Narrator and so on — not one movie credit among
	// them, which is what identified the cause.
	w.notePeople(s.Creator, s.Starring, s.Composer,
		s.Director, s.Producer, s.ExecutiveProducer, s.Writer,
		s.Editor, s.Cinematography, s.Presenter, s.Narrator)
	for _, se := range s.Seasons {
		for _, e := range se.Episodes {
			w.notePeople(e.DirectedBy, e.WrittenBy)
		}
	}
}

// flushSchedules resolves every slot's show to a page_id and stores the grids.
func (w *recordWriter) flushSchedules(cachePath string) (grids, slots, linked int) {
	if len(w.schedules) == 0 {
		return
	}
	db, err := sql.Open("sqlite", cachePath)
	if err != nil {
		w.fail(err)
		return
	}
	defer db.Close()
	stmt, err := db.Prepare(`SELECT page_id FROM wiki_qid WHERE title = ?`)
	if err != nil {
		w.fail(err)
		return
	}
	defer stmt.Close()
	lookup := func(title string) int {
		var id int
		stmt.QueryRow(title).Scan(&id)
		return id
	}
	for _, sc := range w.schedules {
		ResolveShows(sc, lookup)
		for _, e := range sc.Entries {
			slots++
			if e.ShowID != 0 {
				linked++
			}
		}
		w.store.put(filmstock.KindSchedule, int64(sc.PageID), sc)
		grids++
	}
	return grids, slots, linked
}

// flushPeople resolves each person's Q-id from the link target and writes one
// record per identity.
//
// Resolution goes ONLY through the link target. Looking a bare display name up
// as an article title would attach every unlinked "John Smith" to whoever holds
// that title — an invented identity, and silently wrong.
func (w *recordWriter) flushPeople(cachePath string) (withQID, withBio, total, noIdentity int) {
	db, err := sql.Open("sqlite", cachePath)
	if err != nil {
		w.fail(err)
		return
	}
	defer db.Close()
	stmt, err := db.Prepare(`SELECT qid, page_id FROM wiki_qid WHERE title = ?`)
	if err != nil {
		w.fail(err)
		return
	}
	defer stmt.Close()

	var wereWorks int
	for _, p := range w.people {
		// A work is not a person. Infoboxes link works into credit fields —
		// "Saul of the Mole Men" lists itself under starring, and 60 Minutes,
		// The Flintstones and Annie Hall are all credited somewhere as people.
		// The article title says otherwise, and only a pass that has seen the
		// whole corpus can say so, which is exactly what this one has.
		//
		// Unless the article is ALSO a biography: a page claimed as both is a
		// person the film parser mistook for a film, and the biography is the
		// better evidence.
		key := wikitext.CanonTitle(p.Wiki)
		if w.workTitle[key] {
			if _, alsoPerson := w.bios[key]; !alsoPerson {
				wereWorks++
				continue
			}
		}
		var q, pid int64
		if stmt.QueryRow(p.Wiki).Scan(&q, &pid) == nil && q > 0 {
			p.QID = q
			withQID++
		}
		// The dump is authoritative for page_id and covers articles that have no
		// Wikidata item; wiki_qid fills in anyone whose article was not parsed as
		// a biography (not person-shaped, a disambiguation page, a redirect).
		if bp, ok := w.bioPage[wikitext.CanonTitle(p.Wiki)]; ok && bp > 0 {
			p.PageID = bp
		} else if pid > 0 {
			p.PageID = int(pid)
		}
		// Join the biography read from this person's own article. A miss is
		// ordinary and is counted, not guessed at: the credit may link to a
		// redlink, a disambiguation page, or something that is not a person.
		if b, ok := w.bios[wikitext.CanonTitle(p.Wiki)]; ok {
			p.PersonBio = b
			withBio++
		}
		// The display name comes from the article title, not from whichever
		// credit was recorded first: with parsing spread across workers "first"
		// meant arrival order, and the same person got a different name on
		// different runs over identical input.
		p.Name = filmstock.CleanPersonName(p.Wiki)
		p.WikiURL = "https://en.wikipedia.org/wiki/" +
			strings.ReplaceAll(url.PathEscape(p.Wiki), "%20", "_")
		// No article means no page_id and no Q-id: nothing canonical to key on.
		// Such a credit gets no record.
		//
		// It loses nothing. The record held name, wiki and a wikipedia_url —
		// the first two are already on every work that credits the person, and
		// the third pointed at a page that does not exist. The credit itself
		// survives where it belongs, on the film, and the index still builds a
		// searchable person row and their credits from there.
		//
		// It relinks by itself. The credit stores the link TARGET, so the day
		// somebody writes the article a daily update brings that page in, its
		// page_id comes straight from the dump, and the credit resolves to a
		// real record with no rekeying and nothing to migrate.
		//
		// What this removes is the one identity in the database that was not
		// canonical: a 31-bit hash of a display string, which put Issa
		// Abdessamie and Costache Ciubotaru in the same record and would have
		// changed the moment either article was created.
		if p.PageID == 0 {
			noIdentity++
			continue
		}
		w.store.put(filmstock.KindPerson, int64(p.PageID), p)
		total++
	}
	if wereWorks > 0 {
		fmt.Fprintf(os.Stderr, "  %d credits dropped: the link target is a film, series or event\n", wereWorks)
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

	// More than one series can name the same episode-list article: 51 lists in
	// the corpus are claimed by 530 series between them. Writing straight into
	// the map keeps whichever series Go's randomised map iteration reached last,
	// so the owner changed between two runs over identical input — which is how
	// I Love Lucy's six seasons attached to it on one run and to The Lucy–Desi
	// Comedy Hour on the next, with nothing in either output saying so.
	//
	// The articles themselves say which claim is stronger. A series linking
	// "List of I Love Lucy episodes" claims the article; one linking
	// "List of I Love Lucy episodes#The Lucy–Desi Comedy Hour episodes" claims a
	// section of it. So an unfragmented claim wins, and among equals the lowest
	// page_id wins — arbitrary, but fixed, and a page_id rather than a title.
	best := map[int]listClaim{}
	var contested int
	for seriesID, s := range coll.series {
		raw := strings.TrimSpace(s.ListEpisodes)
		t := wikitext.CanonTitle(raw)
		if t == "" {
			continue
		}
		var pid int
		if stmt.QueryRow(t).Scan(&pid) != nil || pid == 0 {
			continue
		}
		c := listClaim{series: seriesID, fragment: strings.Contains(raw, "#")}
		cur, seen := best[pid]
		if !seen {
			best[pid] = c
			continue
		}
		contested++
		if betterClaim(c, cur) {
			best[pid] = c
		}
	}
	out := make(map[int]int, len(best))
	for pid, c := range best {
		out[pid] = c.series
	}
	fmt.Fprintf(os.Stderr, "episode-list links resolved: %d (%d contested)\n", len(out), contested)
	return out, nil
}

// A listClaim is one series' claim on an episode-list article.
type listClaim struct {
	series   int
	fragment bool // the link named a section rather than the whole article
}

// betterClaim reports whether a should own the list article rather than b.
// Whole-article claims beat section claims; ties break on the lower page_id so
// the answer is the same on every run.
func betterClaim(a, b listClaim) bool {
	if a.fragment != b.fragment {
		return !a.fragment
	}
	return a.series < b.series
}

// isTerminal reports whether w is a terminal, so progress can rewrite one line
// there and emit whole lines when redirected. A \r-only progress line vanishes
// into a pipe's buffer, which is not a progress indicator.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
