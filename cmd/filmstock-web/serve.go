// Command filmstock-web is the browser: search the database, open a record.
//
// It is built on the public github.com/szatmary/filmstock API and nothing else.
package main

import (
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/szatmary/filmstock"
	"github.com/szatmary/filmstock/internal/query"
	"github.com/szatmary/filmstock/internal/record"
	"github.com/szatmary/filmstock/internal/sqldrv"
)

type server struct {
	fs   *filmstock.DB
	text *sql.DB // filmstock-text.db, nil when absent
	ex   *explorers
}

func main() {
	dbPath := flag.String("db", "filmstock.db", "the database")
	textPath := flag.String("text-db", "", "synopsis database (default <db>'s sibling filmstock-text.db)")
	vectors := flag.String("vectors", "", "embedding vectors, to enable /explore")
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	db, err := filmstock.Open(*dbPath)
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	s := &server{fs: db}
	explicitText := *textPath != ""
	if !explicitText {
		*textPath = strings.TrimSuffix(*dbPath, ".db") + "-text.db"
	}
	if _, err := os.Stat(*textPath); err == nil {
		t, err := sql.Open(sqldrv.Name, "file:"+*textPath+"?mode=ro")
		if err != nil {
			fatal(err)
		}
		defer t.Close()
		s.text = t
	} else if explicitText {
		fatal(fmt.Errorf("text database %s: %w", *textPath, err))
	} else {
		fmt.Fprintf(os.Stderr, "no text database at %s; synopses and plots will be absent\n", *textPath)
	}
	if *vectors != "" {
		v, err := query.OpenVectors(*vectors)
		if err != nil {
			fatal(err)
		}
		ids, langs, decades, err := filmIDs(*dbPath)
		if err != nil {
			fatal(err)
		}
		s.ex = newExplorers(v, ids, langs, decades)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/search", s.handleAPISearch)
	mux.HandleFunc("/api/people", s.handleAPIPeople)
	mux.HandleFunc("/api/all", s.handleAPIAll)
	mux.HandleFunc("/api/television", s.handleAPITelevision)
	mux.HandleFunc("/api/episodes", s.handleAPIEpisodes)
	mux.HandleFunc("/api/events", s.handleAPIEvents)
	mux.HandleFunc("/movie", s.handleMovie)
	mux.HandleFunc("/television", s.handleTelevision)
	mux.HandleFunc("/person", s.handlePerson)
	mux.HandleFunc("/event", s.handleEventPage)
	if s.ex != nil {
		mux.HandleFunc("/explore", s.handleExplore)
		mux.HandleFunc("/api/explore", s.handleAPIExplore)
		mux.HandleFunc("/api/synopsis", s.handleAPISynopsis)
	}

	if s.ex == nil {
		fmt.Fprintln(os.Stderr, "explorer: disabled (pass -vectors FILE to enable /explore)")
	}
	fmt.Fprintf(os.Stderr, "filmstock browser listening on http://localhost%s\n", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		fatal(err)
	}
}

// filmIDs lists every film in the database, with its primary language, so the
// explorer can be restricted to records it can display and can tell when a
// whole neighbourhood speaks one language.
func filmIDs(dbPath string) ([]int, map[int]string, map[int]string, error) {
	db, err := sql.Open(sqldrv.Name, dbPath)
	if err != nil {
		return nil, nil, nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, language, year FROM movies`)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	var out []int
	langs := map[int]string{}
	decades := map[int]string{}
	for rows.Next() {
		var id, year int
		var lang string
		if err := rows.Scan(&id, &lang, &year); err != nil {
			return nil, nil, nil, err
		}
		out = append(out, id)
		if i := strings.IndexByte(lang, '·'); i > 0 {
			lang = lang[:i]
		}
		if lang = strings.TrimSpace(lang); lang != "" {
			langs[id] = lang
		}
		if year > 1870 {
			decades[id] = fmt.Sprintf("%d0s", year/10)
		}
	}
	return out, langs, decades, rows.Err()
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "fatal:", err)
	os.Exit(1)
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages.ExecuteTemplate(w, "index.html", nil); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func (s *server) handleAPISearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	field := r.URL.Query().Get("field")
	if field == "" {
		field = "title"
	}
	results, err := query.SearchMovies(r.Context(), s.fs.SQL(), q, field, 25)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

func (s *server) handleAPITelevision(w http.ResponseWriter, r *http.Request) {
	field := r.URL.Query().Get("field")
	results, err := query.SearchTelevision(r.Context(), s.fs.SQL(), r.URL.Query().Get("q"), field, 25)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

var typeTie = map[string]int{"television": 0, "movie": 1, "event": 2, "person": 3, "episode": 4}

func (s *server) handleAPIAll(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	ctx := r.Context() // one context for all 4 sub-searches: they cancel together
	var mv []query.SearchResult
	var television []query.TelevisionSearchResult
	var ep []query.EpisodeSearchResult
	var pe []query.PersonResult
	var ev []query.UnifiedResult
	var wg sync.WaitGroup
	wg.Add(5)
	go func() { defer wg.Done(); mv, _ = query.SearchMovies(ctx, s.fs.SQL(), q, "title", 8) }()
	go func() { defer wg.Done(); television, _ = query.SearchTelevision(ctx, s.fs.SQL(), q, "title", 8) }()
	go func() { defer wg.Done(); ep, _ = query.SearchEpisodes(ctx, s.fs.SQL(), q, 6) }()
	go func() { defer wg.Done(); pe, _ = query.SearchPeople(ctx, s.fs.SQL(), q, 6) }()
	go func() { defer wg.Done(); ev, _ = query.SearchEvents(ctx, s.fs.SQL(), q, 4) }()
	wg.Wait()

	var out []query.UnifiedResult
	yr := func(y int) string {
		if y > 0 {
			return fmt.Sprintf(" (%d)", y)
		}
		return ""
	}
	for _, m := range mv {
		out = append(out, query.UnifiedResult{Type: "movie", Title: m.Title + yr(m.Year), Subtitle: m.Director, Link: fmt.Sprintf("/movie?id=%d", m.ID), Cover: m.Cover, Score: m.Score})
	}
	for _, t := range television {
		sub := fmt.Sprintf("%d seasons · %d episodes", t.SeasonsCount, t.EpisodesCount)
		if t.Network != "" {
			sub += " · " + t.Network
		}
		out = append(out, query.UnifiedResult{Type: "television", Title: t.Title + yr(t.Year), Subtitle: sub, Link: fmt.Sprintf("/television?id=%d", t.ID), Cover: t.Cover, Score: t.Score})
	}
	// Episodes: dedup by title (many shows share "Pilot"/"Community") and cap,
	// so a handful don't crowd out shows, movies, and people.
	epSeen := map[string]bool{}
	epShown := 0
	for _, e := range ep {
		key := strings.ToLower(e.Title)
		if epSeen[key] {
			continue
		}
		epSeen[key] = true
		sub := e.SeriesTitle
		if e.Season > 0 {
			sub += fmt.Sprintf(" · S%dE%d", e.Season, e.NumberInSeason)
		}
		out = append(out, query.UnifiedResult{Type: "episode", Title: e.Title, Subtitle: sub, Link: fmt.Sprintf("/television?id=%d", e.SeriesID), Cover: "", Score: e.Score})
		if epShown++; epShown >= 3 {
			break
		}
	}
	out = append(out, ev...)
	for _, p := range pe {
		out = append(out, query.UnifiedResult{Type: "person", Title: p.Name, Subtitle: fmt.Sprintf("%d credits", p.Credits), Link: fmt.Sprintf("/person?id=%d", p.ID), Cover: "", Score: p.Score})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if d := out[i].Score - out[j].Score; d > 0.02 || d < -0.02 {
			return out[i].Score > out[j].Score
		}
		return typeTie[out[i].Type] < typeTie[out[j].Type]
	})
	if len(out) > 25 {
		out = out[:25]
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (s *server) handleAPIEpisodes(w http.ResponseWriter, r *http.Request) {
	results, err := query.SearchEpisodes(r.Context(), s.fs.SQL(), r.URL.Query().Get("q"), 30)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

func (s *server) handleTelevision(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.URL.Query().Get("id"))
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	series, err := s.televisionView(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "series not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "could not load series: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages.ExecuteTemplate(w, "television.html", series); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func (s *server) handleAPIPeople(w http.ResponseWriter, r *http.Request) {
	results, err := query.SearchPeople(r.Context(), s.fs.SQL(), r.URL.Query().Get("q"), 25)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

func (s *server) handlePerson(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.URL.Query().Get("id"))
	if id == 0 {
		if wiki := r.URL.Query().Get("wiki"); wiki != "" {
			id = query.PersonIDByWiki(s.fs.SQL(), wiki)
		}
	}
	if id == 0 {
		if name := r.URL.Query().Get("name"); name != "" {
			id = query.PersonIDByName(s.fs.SQL(), name)
		}
	}
	if id == 0 {
		http.Error(w, "person not found", 404)
		return
	}
	fm, err := query.PersonFilmography(s.fs.SQL(), id)
	if err != nil {
		http.Error(w, "person not found", 404)
		return
	}
	// The biography comes from the person's own article, via the record. Absent
	// for anyone whose credit links to a redlink or something that is not a
	// person; the page then shows identity and filmography, as it always did.
	rec, err := query.PersonByID(r.Context(), s.fs.SQL(), id)
	if err != nil {
		rec = nil
	}
	// PersonBio is embedded by pointer, so rec.Image promotes straight through a
	// nil PersonBio and panics. Pull the biography out once, explicitly, and work
	// from that — every access below is then guarded by one nil check.
	var bio *record.PersonBio
	var wikiURL string
	if rec != nil {
		bio, wikiURL = rec.PersonBio, rec.WikiURL
	}
	if bio != nil && bio.Image != "" {
		fm.Image = record.FilePathURL(bio.Image, 250)
	} else {
		// No portrait in the infobox: fall back to the live thumbnail lookup.
		fm.Image = query.FetchPersonImage(fm.Name)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The template gets the biography pointer itself, not the record: {{with}}
	// guards a nil Bio, but would not guard a non-nil record whose PersonBio is
	// nil — which is every person until their article has been extracted.
	if err := pages.ExecuteTemplate(w, "person.html", struct {
		*query.Filmography
		Bio     *record.PersonBio
		WikiURL string
	}{fm, bio, wikiURL}); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func (s *server) handleMovie(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.URL.Query().Get("id"))
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	m, err := s.movieView(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "movie not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "could not load movie: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages.ExecuteTemplate(w, "movie.html", m); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

var funcs = template.FuncMap{
	"join":            func(v []string) string { return strings.Join(v, ", ") },
	"title":           record.CleanTitle,
	"televisiontitle": record.CleanTelevisionTitle,
	// namehref links a display name to its person page. Names are not
	// identities; the person handler resolves or 404s honestly.
	"namehref": func(name string) string { return "/person?name=" + url.QueryEscape(name) },
}

//go:embed templates/*.html
var templatesFS embed.FS

// pages holds all page templates, parsed from the embedded templates/ dir.
var pages = template.Must(template.New("").Funcs(funcs).ParseFS(templatesFS, "templates/*.html"))
