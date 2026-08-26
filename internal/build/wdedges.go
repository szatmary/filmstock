package build

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
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

// External identifiers, published as the join keys they are.
//
// These are statements ON Wikidata — CC0 facts about a work — not content taken
// from the services they name. Shipping the identifier lets a consumer join
// whatever they are separately licensed to hold: IMDb's non-commercial datasets
// are for personal use only and could never be redistributed here, but anyone
// entitled to a copy can ATTACH it and join on imdb_id in one statement.
//
// So filmstock supplies the key and never the data, which keeps every byte we
// publish under Wikipedia's and Wikidata's own licences.
var externalIDProps = map[string]string{
	"P345":  "imdb",
	"P4947": "tmdb_movie",
	"P4983": "tmdb_tv",
	"P4835": "tvdb",
	"P1258": "rotten_tomatoes",
}

// wdEntity decodes only what we need. Claims stays raw so that the decoder does
// not build structures for the hundreds of properties on a typical item; only
// the two properties we care about are unmarshalled further.
type wdEntity struct {
	ID        string                     `json:"id"`
	Claims    map[string]json.RawMessage `json:"claims"`
	Sitelinks map[string]struct {
		Title string `json:"title"`
	} `json:"sitelinks"`
}

// wdStringClaim reads an external-identifier claim, whose datavalue is a plain
// string rather than the entity reference wdClaim expects.
type wdStringClaim struct {
	Rank     string `json:"rank"`
	Mainsnak struct {
		SnakType  string `json:"snaktype"`
		DataValue struct {
			Value string `json:"value"`
		} `json:"datavalue"`
	} `json:"mainsnak"`
}

// claimStrings returns the identifier values for a property, skipping
// deprecated statements and valueless snaks.
func claimStrings(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var claims []wdStringClaim
	if json.Unmarshal(raw, &claims) != nil {
		return nil
	}
	var out []string
	for _, c := range claims {
		if c.Rank == "deprecated" || c.Mainsnak.SnakType != "value" {
			continue
		}
		if v := strings.TrimSpace(c.Mainsnak.DataValue.Value); v != "" {
			out = append(out, v)
		}
	}
	return out
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

// CBuildWDEdges streams the Wikidata entity dump on stdin (one JSON entity per
// line) and writes the stated relations we key on into the given SQLite db.
//
// Both properties are extracted in a single pass on purpose: decompressing the
// ~102 GB dump is by far the dominant cost, so a second pass to pick up another
// property would cost as much as the first.
func CBuildWDEdges(args []string) {
	fs := flag.NewFlagSet("build-wd-edges", flag.ExitOnError)
	dbPath := fs.String("db", "wikidata.db", "sqlite db to write the edge tables into")
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
	// Drop any wd_text left by an older build; see the note below.
	db.Exec(`DROP TABLE IF EXISTS wd_text`)
	if _, err := db.Exec(`CREATE TABLE wd_part_of_series(item_qid INTEGER, series_qid INTEGER)`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE TABLE wd_external_id(item_qid INTEGER, source TEXT, value TEXT)`); err != nil {
		fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE wd_episode_season(item_qid INTEGER, season_qid INTEGER)`); err != nil {
		return err
	}
	// A wd_text table of multilingual labels, descriptions and aliases used to be
	// built here, for every entity with an enwiki sitelink. It was 25.16 GB —
	// 96% of the resolver cache — and nothing ever read it: no SELECT against it
	// existed on any branch or in any commit. It cost most of phase 1's write I/O
	// and made the cache far too large to move, cache in CI, or keep around.
	//
	// It is not built any more. Multilingual titles would be a real feature, and
	// when something needs them this comes back with a consumer attached.

	needle179 := []byte(`"` + propPartOfSeries + `":`)
	needle4908 := []byte(`"` + propSeason + `":`)
	needleEnwiki := []byte(`"enwiki":`)
	// One needle per external-identifier property, in a fixed order so the
	// prefilter costs the same on every run.
	extProps := make([]string, 0, len(externalIDProps))
	for p := range externalIDProps {
		extProps = append(extProps, p)
	}
	sort.Strings(extProps)
	needlesExt := make([][]byte, 0, len(extProps))
	for _, p := range extProps {
		needlesExt = append(needlesExt, []byte(`"`+p+`":`))
	}

	// Decompression (lbzip2, ~860 MB/s across 20 cores) outruns a single-threaded
	// scanner, so the line parsers are fanned out and only the SQLite writer is
	// serial. Reading stays on one goroutine to preserve stdin order cheaply.
	type edge struct {
		season bool // false = P179 part-of-series, true = P4908 episode-season
		from   int64
		to     int64
		// An external identifier instead of a relation, when source is set.
		source string
		value  string
	}
	lines := make(chan []byte, 4096)
	edges := make(chan edge, 8192)

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
				hasExt := false
				for _, nd := range needlesExt {
					if bytes.Contains(line, nd) {
						hasExt = true
						break
					}
				}
				if !has179 && !has4908 && !hasEn && !hasExt {
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
						edges <- edge{from: from, to: to}
					}
				}
				if has4908 {
					for _, to := range claimTargets(e.Claims[propSeason]) {
						edges <- edge{season: true, from: from, to: to}
					}
				}
				if hasExt {
					// In sorted property order: ranging the map would emit the
					// identifiers in a different order on every run, and this
					// table is published.
					for _, prop := range extProps {
						for _, v := range claimStrings(e.Claims[prop]) {
							edges <- edge{from: from, source: externalIDProps[prop], value: v}
						}
					}
				}
			}
		}()
	}
	go func() { wg.Wait(); close(edges) }()

	start := time.Now()
	done := make(chan struct{})
	var nSeries, nSeason, nExt int64
	go func() {
		defer close(done)
		tx, _ := db.Begin()
		prep := func() (*sql.Stmt, *sql.Stmt, *sql.Stmt) {
			a, _ := tx.Prepare(`INSERT INTO wd_part_of_series(item_qid,series_qid) VALUES(?,?)`)
			b, _ := tx.Prepare(`INSERT INTO wd_episode_season(item_qid,season_qid) VALUES(?,?)`)
			c, _ := tx.Prepare(`INSERT INTO wd_external_id(item_qid,source,value) VALUES(?,?,?)`)
			return a, b, c
		}
		insSeries, insSeason, insExt := prep()
		n := 0
		bump := func() {
			// Commit periodically so a long run is not one giant transaction.
			if n++; n%500_000 == 0 {
				insSeries.Close()
				insSeason.Close()
				insExt.Close()
				if err := tx.Commit(); err != nil {
					panic(err)
				}
				tx, _ = db.Begin()
				insSeries, insSeason, insExt = prep()
			}
		}
		for edges != nil {
			select {
			case e, ok := <-edges:
				if !ok {
					edges = nil
					continue
				}
				switch {
				case e.source != "":
					insExt.Exec(e.from, e.source, e.value)
					atomic.AddInt64(&nExt, 1)
				case e.season:
					insSeason.Exec(e.from, e.to)
					atomic.AddInt64(&nSeason, 1)
				default:
					insSeries.Exec(e.from, e.to)
					atomic.AddInt64(&nSeries, 1)
				}
				bump()
			}
		}
		insSeries.Close()
		insSeason.Close()
		insExt.Close()
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
				fmt.Fprintf(os.Stderr, "  %d entities (%.0f/s)  P179=%d  P4908=%d\n",
					n, float64(n)/el, atomic.LoadInt64(&nSeries), atomic.LoadInt64(&nSeason))
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
	db.Exec(`CREATE INDEX idx_wd_ext_item ON wd_external_id(item_qid)`)

	fmt.Fprintf(os.Stderr,
		"DONE: entities=%d parsed=%d P179=%d P4908=%d external_ids=%d elapsed=%.1fm\n",
		entities, parsed, nSeries, nSeason, nExt, time.Since(start).Minutes())
	return nil
}
