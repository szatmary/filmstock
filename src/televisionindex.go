package main

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

const televisionSchema = `
DROP TABLE IF EXISTS television_series;
DROP TABLE IF EXISTS television_fts;
DROP TABLE IF EXISTS television_episodes;
DROP TABLE IF EXISTS television_episodes_fts;
DELETE FROM credits WHERE work_type='television';
CREATE TABLE television_series(
  id INTEGER PRIMARY KEY, title TEXT NOT NULL, year INTEGER,
  first_aired TEXT, last_aired TEXT, genre TEXT, creator TEXT, starring TEXT,
  network TEXT, num_seasons TEXT, num_episodes TEXT,
  seasons_count INTEGER, episodes_count INTEGER, overview TEXT, plot TEXT,
  cover_image_file TEXT, wikipedia_url TEXT, path TEXT NOT NULL
);
CREATE VIRTUAL TABLE television_fts USING fts5(
  title, starring, creator,
  content='television_series', content_rowid='id', tokenize='trigram'
);
CREATE TABLE television_episodes(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  series_id INTEGER, series_title TEXT, season INTEGER,
  number_in_season INTEGER, number_overall INTEGER, title TEXT, air_date TEXT, summary TEXT
);
CREATE INDEX idx_television_ep_series ON television_episodes(series_id);
CREATE VIRTUAL TABLE television_episodes_fts USING fts5(
  title, content='television_episodes', content_rowid='id', tokenize='trigram'
);`

func readTelevisionSeriesGz(path string) (*TelevisionSeries, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	data, err := io.ReadAll(gz)
	if err != nil {
		return nil, err
	}
	var s TelevisionSeries
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// cmdIndexTelevision adds television_series + television_fts tables to an existing database, indexing the
// per-series JSON.gz files. Does not touch the movies tables.
func cmdIndexTelevision(args []string) {
	fs := flag.NewFlagSet("index-television", flag.ExitOnError)
	televisionDir := fs.String("television", "../television", "directory of per-series JSON.gz files")
	dbPath := fs.String("db", "../movies.db", "SQLite database (shared with movies)")
	records := fs.String("records", "", "record hierarchy from `extract` (supplies people identities)")
	workers := fs.Int("workers", 16, "reader workers")
	fs.Parse(args)

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(televisionSchema); err != nil {
		fatal(err)
	}
	// Identities come from the person records, not from a dump or resolver.
	p2q, err := loadPeopleQIDs(*records)
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "  %d person identities from records\n", len(p2q))

	var files []string
	filepath.WalkDir(*televisionDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".json.gz") {
			files = append(files, p)
		}
		return nil
	})
	fmt.Fprintf(os.Stderr, "indexing %d television series into %s...\n", len(files), *dbPath)

	type item struct {
		s    *TelevisionSeries
		path string
	}
	paths := make(chan string, 2048)
	items := make(chan item, 2048)
	var rwg sync.WaitGroup
	for i := 0; i < *workers; i++ {
		rwg.Add(1)
		go func() {
			defer rwg.Done()
			for p := range paths {
				s, err := readTelevisionSeriesGz(p)
				if err != nil {
					continue
				}
				rel, _ := filepath.Rel(*televisionDir, p)
				items <- item{s, rel}
			}
		}()
	}
	go func() {
		for _, p := range files {
			paths <- p
		}
		close(paths)
		rwg.Wait()
		close(items)
	}()

	tx, _ := db.Begin()
	stmt, _ := tx.Prepare(`INSERT OR REPLACE INTO television_series
		(id,title,year,first_aired,last_aired,genre,creator,starring,network,
		 num_seasons,num_episodes,seasons_count,episodes_count,overview,plot,cover_image_file,wikipedia_url,path)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	epStmt, _ := tx.Prepare(`INSERT INTO television_episodes
		(series_id,series_title,season,number_in_season,number_overall,title,air_date,summary) VALUES(?,?,?,?,?,?,?,?)`)
	// Reuses movie-pass person ids (loads existing people); resolves Q-ids.
	pb, err := newPeopleBuilder(tx, p2q)
	if err != nil {
		fatal(err)
	}

	n, nEp := 0, 0
	for it := range items {
		s := it.s
		year := 0
		if len(s.FirstAired) >= 4 {
			fmt.Sscanf(s.FirstAired[:4], "%d", &year)
		}
		epCount := 0
		for _, se := range s.Seasons {
			epCount += len(se.Episodes)
		}
		cleanName := cleanTelevisionTitle(s.Title)
		stmt.Exec(s.PageID, cleanName, year, s.FirstAired, s.LastAired,
			join(s.Genre), joinP(s.Creator), joinP(s.Starring), join(s.Network),
			s.NumSeasons, s.NumEpisodes, len(s.Seasons), epCount,
			s.Overview, s.Plot, s.CoverImageFile, s.WikiURL, it.path)

		seen := map[string]bool{}
		pb.credit(seen, s.Creator, s.PageID, "television", "Creator")
		pb.credit(seen, s.Starring, s.PageID, "television", "Cast")
		pb.credit(seen, s.Composer, s.PageID, "television", "Composer")
		for _, se := range s.Seasons {
			for _, e := range se.Episodes {
				pb.credit(seen, e.DirectedBy, s.PageID, "television", "Director")
				pb.credit(seen, e.WrittenBy, s.PageID, "television", "Writer")
				epStmt.Exec(s.PageID, cleanName, se.Season, e.NumberInSeason, e.NumberOverall, e.Title, e.AirDate, e.Summary)
				nEp++
			}
		}
		n++
	}
	stmt.Close()
	epStmt.Close()
	tx.Commit()
	fmt.Fprintf(os.Stderr, "  inserted %d series, %d episodes. building FTS...\n", n, nEp)
	for _, t := range []string{"television_fts", "television_episodes_fts", "people_fts"} {
		if _, err := db.Exec(`INSERT INTO ` + t + `(` + t + `) VALUES('rebuild')`); err != nil {
			fatal(err)
		}
	}
	fmt.Fprintln(os.Stderr, "done.")
}

// TelevisionSearchResult is one ranked television series hit.
type TelevisionSearchResult struct {
	ID            int
	Title         string
	Year          int
	Network       string
	Starring      string
	Cover         string
	Path          string
	SeasonsCount  int
	EpisodesCount int
	Score         float64
}

// EpisodeSearchResult is one ranked episode hit.
type EpisodeSearchResult struct {
	Title          string
	SeriesID       int
	SeriesTitle    string
	Season         int
	NumberInSeason int
	AirDate        string
	Score          float64
}

// searchEpisodes fuzzy-searches television episode titles.
func searchEpisodes(ctx context.Context, db *sql.DB, query string, limit int) ([]EpisodeSearchResult, error) {
	qgrams := trigrams(normalize(query))
	if len(qgrams) == 0 {
		return nil, nil
	}
	qTokens := strings.Fields(normalize(query))
	parts := make([]string, 0, len(qgrams))
	for g := range qgrams {
		parts = append(parts, `"`+strings.ReplaceAll(g, `"`, `""`)+`"`)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT e.title,e.series_id,e.series_title,e.season,e.number_in_season,e.air_date
		FROM television_episodes_fts f JOIN television_episodes e ON e.id=f.rowid
		WHERE television_episodes_fts MATCH ?
		ORDER BY bm25(television_episodes_fts) LIMIT 800`, strings.Join(parts, " OR "))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []EpisodeSearchResult
	for rows.Next() {
		var r EpisodeSearchResult
		var season, num sql.NullInt64
		if err := rows.Scan(&r.Title, &r.SeriesID, &r.SeriesTitle, &season, &num, &r.AirDate); err != nil {
			continue
		}
		r.Season, r.NumberInSeason = int(season.Int64), int(num.Int64)
		r.SeriesTitle = cleanTelevisionTitle(r.SeriesTitle)
		r.Score = fuzzyScore(qgrams, qTokens, r.Title)
		res = append(res, r)
	}
	sort.SliceStable(res, func(i, j int) bool { return res[i].Score > res[j].Score })
	if limit > 0 && len(res) > limit {
		res = res[:limit]
	}
	return res, nil
}

// searchTelevision fuzzy-searches the television series index, mirroring searchMovies.
func searchTelevision(ctx context.Context, db *sql.DB, query, field string, limit int) ([]TelevisionSearchResult, error) {
	qgrams := trigrams(normalize(query))
	if len(qgrams) == 0 {
		return nil, nil
	}
	qTokens := strings.Fields(normalize(query))
	parts := make([]string, 0, len(qgrams))
	for g := range qgrams {
		parts = append(parts, `"`+strings.ReplaceAll(g, `"`, `""`)+`"`)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT t.id,t.title,t.year,t.network,t.starring,t.cover_image_file,t.path,t.seasons_count,t.episodes_count
		FROM television_fts f JOIN television_series t ON t.id=f.rowid
		WHERE television_fts MATCH ?
		ORDER BY bm25(television_fts, 10.0, 2.0, 2.0)
		LIMIT 500`, strings.Join(parts, " OR "))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []TelevisionSearchResult
	for rows.Next() {
		var r TelevisionSearchResult
		var year sql.NullInt64
		var coverFile string
		if err := rows.Scan(&r.ID, &r.Title, &year, &r.Network, &r.Starring, &coverFile, &r.Path, &r.SeasonsCount, &r.EpisodesCount); err != nil {
			continue
		}
		r.Year = int(year.Int64)
		r.Cover = filePathURL(coverFile, 0)
		target := cleanTelevisionTitle(r.Title)
		if field == "starring" {
			target = r.Starring
		}
		r.Score = fuzzyScore(qgrams, qTokens, target)
		r.Title = cleanTelevisionTitle(r.Title) // display without the (TV series) suffix
		res = append(res, r)
	}
	sort.SliceStable(res, func(i, j int) bool { return res[i].Score > res[j].Score })
	if limit > 0 && len(res) > limit {
		res = res[:limit]
	}
	return res, nil
}
