package main

import (
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA journal_mode=OFF;
PRAGMA synchronous=OFF;
CREATE TABLE movies(
  id INTEGER PRIMARY KEY,
  title TEXT NOT NULL,
  year INTEGER,
  release_date TEXT,
  director TEXT, producer TEXT, writer TEXT, starring TEXT,
  music TEXT, distributor TEXT, country TEXT, language TEXT,
  runtime TEXT, budget TEXT, gross TEXT,
  wikipedia_url TEXT, cover_image_url TEXT, cover_image_file TEXT, path TEXT NOT NULL
);
CREATE VIRTUAL TABLE movies_fts USING fts5(
  title, starring, director,
  content='movies', content_rowid='id', tokenize='trigram'
);
`

// roleCredits maps a display role label to the Movie field holding those people.
func roleCredits(m *Movie) []struct {
	role   string
	people []Person
} {
	return []struct {
		role   string
		people []Person
	}{
		{"Director", m.Director},
		{"Writer", m.Writer},
		{"Producer", m.Producer},
		{"Cast", m.Starring},
		{"Composer", m.Music},
		{"Cinematographer", m.Cinematography},
		{"Editor", m.Editing},
	}
}

// indexItem carries a parsed movie plus its store-relative path to the DB writer.
type indexItem struct {
	m    *Movie
	path string
}

func cmdIndex(args []string) {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	moviesDir := fs.String("movies", "out/movies", "directory of per-movie JSON.gz files")
	dbPath := fs.String("db", "out/search.db", "output SQLite database")
	records := fs.String("records", "", "record hierarchy from `extract` (supplies people identities)")
	workers := fs.Int("workers", 16, "reader workers")
	fs.Parse(args)

	if err := os.Remove(*dbPath); err != nil && !os.IsNotExist(err) {
		fatal(err)
	}
	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schema); err != nil {
		fatal(err)
	}
	if _, err := db.Exec(peopleSchema); err != nil {
		fatal(err)
	}
	// Identities come from the person records, not from a dump or resolver.
	p2q, err := loadPeopleQIDs(*records)
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "  %d person identities from records\n", len(p2q))

	var files []string
	filepath.WalkDir(*moviesDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".json.gz") {
			files = append(files, p)
		}
		return nil
	})
	fmt.Fprintf(os.Stderr, "indexing %d movie files into %s...\n", len(files), *dbPath)

	paths := make(chan string, 2048)
	items := make(chan indexItem, 2048)
	var rwg sync.WaitGroup
	for i := 0; i < *workers; i++ {
		rwg.Add(1)
		go func() {
			defer rwg.Done()
			for p := range paths {
				m, err := readMovieGz(p)
				if err != nil {
					fmt.Fprintln(os.Stderr, "read error:", p, err)
					continue
				}
				rel, err := filepath.Rel(*moviesDir, p)
				if err != nil {
					rel = p
				}
				items <- indexItem{m, rel}
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

	// single writer: one big transaction
	tx, err := db.Begin()
	if err != nil {
		fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO movies
		(id,title,year,release_date,director,producer,writer,starring,music,
		 distributor,country,language,runtime,budget,gross,wikipedia_url,cover_image_url,cover_image_file,path)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		fatal(err)
	}
	pb, err := newPeopleBuilder(tx, p2q)
	if err != nil {
		fatal(err)
	}

	n, credits := 0, 0
	for it := range items {
		m := it.m
		if _, err := stmt.Exec(
			m.PageID, m.Title, yearOf(m), first(m.ReleaseDates),
			joinP(m.Director), joinP(m.Producer), joinP(m.Writer), joinP(m.Starring),
			joinP(m.Music), join(m.Distributor), join(m.Country), join(m.Language),
			m.Runtime, m.Budget, m.Gross, m.WikiURL, m.CoverImageURL, m.CoverImageFile, it.path,
		); err != nil {
			fmt.Fprintln(os.Stderr, "insert error:", m.Title, err)
			continue
		}
		seen := map[string]bool{}
		for _, rc := range roleCredits(m) {
			pb.credit(seen, rc.people, m.PageID, "movie", rc.role)
			credits += len(rc.people)
		}
		n++
		if n%20000 == 0 {
			fmt.Fprintf(os.Stderr, "\r  inserted %d movies, %d credits...", n, credits)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "\r  inserted %d movies, %d credits (%d people). building FTS indexes...\n",
		n, credits, len(pb.byKey))
	if _, err := db.Exec(`INSERT INTO movies_fts(movies_fts) VALUES('rebuild')`); err != nil {
		fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO people_fts(people_fts) VALUES('rebuild')`); err != nil {
		fatal(err)
	}
	fmt.Fprintln(os.Stderr, "done.")
}

// readMovieGz reads and decodes one gzip-compressed movie JSON file.
func readMovieGz(path string) (*Movie, error) {
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
	var m Movie
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func join(v []string) string { return strings.Join(v, " · ") }

func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// yearOf extracts a 4-digit year from the first release date, or 0.
func yearOf(m *Movie) int {
	s := first(m.ReleaseDates)
	if len(s) >= 4 {
		y := 0
		for i := 0; i < 4; i++ {
			if s[i] < '0' || s[i] > '9' {
				return 0
			}
			y = y*10 + int(s[i]-'0')
		}
		return y
	}
	return 0
}
