package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// Relations extracted from the Wikidata entity dump. Both are *stated* edges
// between items, which is why they can key data that titles cannot:
//
//	P179  "part of the series"  season -> series   (Lost, season 1 -> Lost)
//	P4908 "season"              episode -> season  (Homecoming -> Lost, season 1)
//
// Note the direction on P4908: its label reads like "the season of a series",
// but the statements run from the episode upward. Verified against Q582972.
const (
	propPartOfSeries = "P179"
	propSeason       = "P4908"
)

// wdEntity decodes only what we need. Claims stays raw so that the decoder does
// not build structures for the hundreds of properties on a typical item; only
// the two properties we care about are unmarshalled further.
type wdEntity struct {
	ID           string                     `json:"id"`
	Claims       map[string]json.RawMessage `json:"claims"`
	Labels       map[string]langVal         `json:"labels"`
	Descriptions map[string]langVal         `json:"descriptions"`
	Aliases      map[string][]langVal       `json:"aliases"`
	Sitelinks    map[string]struct {
		Title string `json:"title"`
	} `json:"sitelinks"`
}

type langVal struct {
	Value string `json:"value"`
}

// compactLang rewrites Wikidata's {"en":{"language":"en","value":"Belgium"}} as
// {"en":"Belgium"}. Same information at roughly a third the bytes, across ~7M
// entities and up to ~300 languages each.
func compactLang(m map[string]langVal) string {
	if len(m) == 0 {
		return ""
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v.Value
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func compactAliases(m map[string][]langVal) string {
	if len(m) == 0 {
		return ""
	}
	out := make(map[string][]string, len(m))
	for k, vs := range m {
		s := make([]string, 0, len(vs))
		for _, v := range vs {
			s = append(s, v.Value)
		}
		out[k] = s
	}
	b, _ := json.Marshal(out)
	return string(b)
}

type wdClaim struct {
	Rank     string `json:"rank"`
	Mainsnak struct {
		SnakType  string `json:"snaktype"`
		DataValue struct {
			Value struct {
				NumericID int64 `json:"numeric-id"`
			} `json:"value"`
		} `json:"datavalue"`
	} `json:"mainsnak"`
}

// qidNum turns "Q582972" into 582972.
func qidNum(id string) (int64, bool) {
	if len(id) < 2 || id[0] != 'Q' {
		return 0, false
	}
	n, err := strconv.ParseInt(id[1:], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// claimTargets returns the numeric ids this property points at, skipping
// deprecated statements and valueless snaks ("unknown value" / "no value").
func claimTargets(raw json.RawMessage) []int64 {
	var claims []wdClaim
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil
	}
	var out []int64
	for _, c := range claims {
		if c.Rank == "deprecated" || c.Mainsnak.SnakType != "value" {
			continue
		}
		if id := c.Mainsnak.DataValue.Value.NumericID; id != 0 {
			out = append(out, id)
		}
	}
	return out
}

// cmdBuildWDEdges streams the Wikidata entity dump on stdin (one JSON entity per
// line) and writes the stated relations we key on into the given SQLite db.
//
// Both properties are extracted in a single pass on purpose: decompressing the
// ~102 GB dump is by far the dominant cost, so a second pass to pick up another
// property would cost as much as the first.
func cmdBuildWDEdges(args []string) {
	fs := flag.NewFlagSet("build-wd-edges", flag.ExitOnError)
	dbPath := fs.String("db", "../wikidata.db", "sqlite db to write the edge tables into")
	every := fs.Int("progress", 250_000, "log progress every N entities")
	workers := fs.Int("workers", 12, "parallel line parsers")
	fs.Parse(args)
	if err := buildWDEdges(os.Stdin, *dbPath, *workers, *every); err != nil {
		fatal(err)
	}
}

// buildWDEdges scans an entity dump (one JSON entity per line) from in, writing
// the relation and multilingual tables. Callable directly so `extract` can drive
// it without a shell pipeline.
func buildWDEdges(in io.Reader, dbPathS string, workersN, everyN int) error {
	dbPath, workers, every := &dbPathS, &workersN, &everyN

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	db.Exec(`PRAGMA journal_mode=OFF; PRAGMA synchronous=OFF;`)
	db.Exec(`DROP TABLE IF EXISTS wd_part_of_series`)
	db.Exec(`DROP TABLE IF EXISTS wd_episode_season`)
	db.Exec(`DROP TABLE IF EXISTS wd_text`)
	if _, err := db.Exec(`CREATE TABLE wd_part_of_series(item_qid INTEGER, series_qid INTEGER)`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE TABLE wd_episode_season(item_qid INTEGER, season_qid INTEGER)`); err != nil {
		return err
	}
	// Multilingual names for anything reachable from this database. Restricted to
	// entities with an enwiki sitelink (~7M of ~115M): the corpus is enwiki-derived,
	// so nothing outside that set can ever be referenced, and the restriction is
	// what keeps this to one row per entity rather than one per language.
	if _, err := db.Exec(`CREATE TABLE wd_text(
		qid INTEGER PRIMARY KEY, enwiki TEXT, labels TEXT, descriptions TEXT, aliases TEXT)`); err != nil {
		return err
	}

	needle179 := []byte(`"` + propPartOfSeries + `":`)
	needle4908 := []byte(`"` + propSeason + `":`)
	needleEnwiki := []byte(`"enwiki":`)

	// Decompression (lbzip2, ~860 MB/s across 20 cores) outruns a single-threaded
	// scanner, so the line parsers are fanned out and only the SQLite writer is
	// serial. Reading stays on one goroutine to preserve stdin order cheaply.
	type edge struct {
		season bool // false = P179 part-of-series, true = P4908 episode-season
		from   int64
		to     int64
	}
	type textRow struct {
		qid                              int64
		enwiki, labels, descs, aliasesJS string
	}
	lines := make(chan []byte, 4096)
	edges := make(chan edge, 8192)
	texts := make(chan textRow, 8192)

	var entities, parsed int64
	var wg sync.WaitGroup
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for line := range lines {
				has179 := bytes.Contains(line, needle179)
				has4908 := bytes.Contains(line, needle4908)
				hasEn := bytes.Contains(line, needleEnwiki)
				if !has179 && !has4908 && !hasEn {
					continue
				}
				line = bytes.TrimRight(line, ",\n\r")
				var e wdEntity
				if json.Unmarshal(line, &e) != nil {
					continue
				}
				from, ok := qidNum(e.ID)
				if !ok {
					continue
				}
				atomic.AddInt64(&parsed, 1)
				if has179 {
					for _, to := range claimTargets(e.Claims[propPartOfSeries]) {
						edges <- edge{false, from, to}
					}
				}
				if has4908 {
					for _, to := range claimTargets(e.Claims[propSeason]) {
						edges <- edge{true, from, to}
					}
				}
				if sl, ok := e.Sitelinks["enwiki"]; ok {
					texts <- textRow{from, sl.Title,
						compactLang(e.Labels), compactLang(e.Descriptions), compactAliases(e.Aliases)}
				}
			}
		}()
	}
	go func() { wg.Wait(); close(edges); close(texts) }()

	start := time.Now()
	done := make(chan struct{})
	var nSeries, nSeason, nText int64
	go func() {
		defer close(done)
		tx, _ := db.Begin()
		prep := func() (*sql.Stmt, *sql.Stmt, *sql.Stmt) {
			a, _ := tx.Prepare(`INSERT INTO wd_part_of_series(item_qid,series_qid) VALUES(?,?)`)
			b, _ := tx.Prepare(`INSERT INTO wd_episode_season(item_qid,season_qid) VALUES(?,?)`)
			c, _ := tx.Prepare(`INSERT OR IGNORE INTO wd_text(qid,enwiki,labels,descriptions,aliases) VALUES(?,?,?,?,?)`)
			return a, b, c
		}
		insSeries, insSeason, insText := prep()
		n := 0
		bump := func() {
			// Commit periodically so a long run is not one giant transaction.
			if n++; n%500_000 == 0 {
				insSeries.Close()
				insSeason.Close()
				insText.Close()
				if err := tx.Commit(); err != nil {
					panic(err)
				}
				tx, _ = db.Begin()
				insSeries, insSeason, insText = prep()
			}
		}
		for edges != nil || texts != nil {
			select {
			case e, ok := <-edges:
				if !ok {
					edges = nil
					continue
				}
				if e.season {
					insSeason.Exec(e.from, e.to)
					atomic.AddInt64(&nSeason, 1)
				} else {
					insSeries.Exec(e.from, e.to)
					atomic.AddInt64(&nSeries, 1)
				}
				bump()
			case t, ok := <-texts:
				if !ok {
					texts = nil
					continue
				}
				insText.Exec(t.qid, t.enwiki, t.labels, t.descs, t.aliasesJS)
				atomic.AddInt64(&nText, 1)
				bump()
			}
		}
		insSeries.Close()
		insSeason.Close()
		insText.Close()
		if err := tx.Commit(); err != nil {
			panic(err)
		}
	}()

	r := bufio.NewReaderSize(in, 8<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			n := atomic.AddInt64(&entities, 1)
			lines <- line
			if n%int64(*every) == 0 {
				el := time.Since(start).Seconds()
				fmt.Fprintf(os.Stderr, "  %d entities (%.0f/s)  P179=%d  P4908=%d  text=%d\n",
					n, float64(n)/el, atomic.LoadInt64(&nSeries), atomic.LoadInt64(&nSeason),
					atomic.LoadInt64(&nText))
			}
		}
		if err != nil {
			if err != io.EOF {
				fmt.Fprintf(os.Stderr, "read error after %d entities: %v\n", entities, err)
			}
			break
		}
	}
	close(lines)
	<-done
	db.Exec(`CREATE INDEX idx_wd_pos_item ON wd_part_of_series(item_qid)`)
	db.Exec(`CREATE INDEX idx_wd_eps_item ON wd_episode_season(item_qid)`)
	db.Exec(`CREATE INDEX idx_wd_text_enwiki ON wd_text(enwiki)`)

	fmt.Fprintf(os.Stderr,
		"DONE: entities=%d parsed=%d P179=%d P4908=%d wd_text=%d elapsed=%.1fm\n",
		entities, parsed, nSeries, nSeason, nText, time.Since(start).Minutes())
	return nil
}
