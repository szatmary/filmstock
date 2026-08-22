package main

import (
	"database/sql"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

type server struct {
	db            *sql.DB
	moviesDir     string
	televisionDir string
	eventsDir     string
	sem           *semanticSearcher
	space         *filmSpace
	late          *colbertSearcher
	routeAt       float64 // lexical-confidence threshold for routing
	lateEmbed     string  // colbert query-token sidecar
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dbPath := fs.String("db", "out/search.db", "SQLite database")
	moviesDir := fs.String("movies", "out/movies", "directory of per-movie JSON.gz files")
	televisionDir := fs.String("television", "out/television", "directory of per-series JSON.gz files")
	eventsDir := fs.String("events", "", "directory of per-event JSON.gz files (award ceremonies, festivals)")
	addr := fs.String("addr", ":8080", "listen address")
	quant := fs.String("quant", "", "quant.<model>.json — enables Semantic mode")
	ids := fs.String("passages", "", "passages.bin (passage -> page_id)")
	embed := fs.String("embedder", "http://localhost:8090/api/embed",
		"query-vector service (embed/testui.py); must run the SAME model as the vectors")
	films := fs.String("films", "", "film vector prefix (films.lead) — enables the space browser")
	fdim := fs.Int("fdim", 1024, "film vector dimension")
	colbert := fs.String("colbert", "", "colbert.<model>.json — enables ColBERT mode")
	colbertEmbed := fs.String("colbert-embedder", "http://localhost:8091/api/colbert",
		"colbert query-token service (embed/colbert.py serve)")
	// 0.60 is the measured peak: MRR 0.609 against 0.575 unrouted. Below it,
	// person queries ("films directed by X") start misrouting into lexical and
	// collapse — 0.469 at 0.60, 0.058 at 0.35 — so the tripwire is well marked.
	routeAt := fs.Float64("route", 0.60,
		"answer from lexical when its best hit scores >= this, else ColBERT (0 disables)")
	fs.Parse(args)

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		fatal(fmt.Errorf("opening %s: %w", *dbPath, err))
	}

	s := &server{db: db, moviesDir: *moviesDir, televisionDir: *televisionDir, eventsDir: *eventsDir, routeAt: *routeAt}
	if *films != "" {
		sp, err := loadFilmSpace(*films+".f32.bin", *films+".ids.bin", *fdim)
		if err != nil {
			fatal(err)
		}
		s.space = sp
		fmt.Fprintf(os.Stderr, "space browser enabled (%d films from %s)\n", len(sp.ids), *films)
	}
	if *quant != "" {
		s.sem = newSemanticSearcher(*embed, *quant, *ids)
		fmt.Fprintf(os.Stderr, "semantic mode enabled (%s via %s)\n", *quant, *embed)
	}
	if *colbert != "" {
		// ColBERT reranks the dense path's candidates, so it cannot run without
		// it. Say so at startup rather than 503ing on the first query.
		if s.sem == nil {
			fatal(fmt.Errorf("-colbert needs -quant: late interaction reranks the dense path's candidates"))
		}
		if err := s.sem.ready(); err != nil {
			fatal(err)
		}
		cb, err := loadColbertIndex(*colbert)
		if err != nil {
			fatal(err)
		}
		s.late, err = newColbertSearcher(s.sem.ix, cb)
		if err != nil {
			fatal(err)
		}
		s.lateEmbed = *colbertEmbed
		fmt.Fprintf(os.Stderr, "colbert mode enabled (%s, %d passages, dim %d, via %s)\n",
			cb.Model, cb.Count, cb.Dim, *colbertEmbed)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/search", s.handleAPISearch)
	mux.HandleFunc("/api/people", s.handleAPIPeople)
	mux.HandleFunc("/movie", s.handleMovie)
	mux.HandleFunc("/person", s.handlePerson)
	mux.HandleFunc("/api/all", s.handleAPIAll)
	mux.HandleFunc("/api/television", s.handleAPITelevision)
	mux.HandleFunc("/api/episodes", s.handleAPIEpisodes)
	mux.HandleFunc("/television", s.handleTelevision)
	mux.HandleFunc("/api/events", s.handleAPIEvents)
	mux.HandleFunc("/event", s.handleEventPage)
	mux.HandleFunc("/api/semantic", s.handleAPISemantic)
	mux.HandleFunc("/api/colbert", s.handleAPIColbert)
	mux.HandleFunc("/api/near", s.handleAPINear)
	mux.HandleFunc("/browse", s.handleBrowse)

	fmt.Fprintf(os.Stderr, "filmstock browser listening on http://localhost%s\n", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		fatal(err)
	}
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
	results, err := searchMovies(r.Context(), s.db, q, field, 25)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

func (s *server) handleAPITelevision(w http.ResponseWriter, r *http.Request) {
	field := r.URL.Query().Get("field")
	results, err := searchTelevision(r.Context(), s.db, r.URL.Query().Get("q"), field, 25)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

// unifiedResult is a type-tagged hit for the combined home-page search.
type unifiedResult struct {
	Type     string // movie | television | episode | person | event
	Title    string
	Subtitle string
	Link     string
	Cover    string
	Score    float64
}

var typeTie = map[string]int{"television": 0, "movie": 1, "event": 2, "person": 3, "episode": 4}

func (s *server) handleAPIAll(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	ctx := r.Context() // one context for all 4 sub-searches: they cancel together
	var mv []SearchResult
	var television []TelevisionSearchResult
	var ep []EpisodeSearchResult
	var pe []PersonResult
	var ev []unifiedResult
	var wg sync.WaitGroup
	wg.Add(5)
	go func() { defer wg.Done(); mv, _ = searchMovies(ctx, s.db, q, "title", 8) }()
	go func() { defer wg.Done(); television, _ = searchTelevision(ctx, s.db, q, "title", 8) }()
	go func() { defer wg.Done(); ep, _ = searchEpisodes(ctx, s.db, q, 6) }()
	go func() { defer wg.Done(); pe, _ = searchPeople(ctx, s.db, q, 6) }()
	go func() { defer wg.Done(); ev, _ = searchEvents(ctx, s.db, q, 4) }()
	wg.Wait()

	var out []unifiedResult
	yr := func(y int) string {
		if y > 0 {
			return fmt.Sprintf(" (%d)", y)
		}
		return ""
	}
	for _, m := range mv {
		out = append(out, unifiedResult{"movie", m.Title + yr(m.Year), m.Director, fmt.Sprintf("/movie?id=%d", m.ID), m.Cover, m.Score})
	}
	for _, t := range television {
		sub := fmt.Sprintf("%d seasons · %d episodes", t.SeasonsCount, t.EpisodesCount)
		if t.Network != "" {
			sub += " · " + t.Network
		}
		out = append(out, unifiedResult{"television", t.Title + yr(t.Year), sub, fmt.Sprintf("/television?id=%d", t.ID), t.Cover, t.Score})
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
		out = append(out, unifiedResult{"episode", e.Title, sub, fmt.Sprintf("/television?id=%d", e.SeriesID), "", e.Score})
		if epShown++; epShown >= 3 {
			break
		}
	}
	out = append(out, ev...)
	for _, p := range pe {
		out = append(out, unifiedResult{"person", p.Name, fmt.Sprintf("%d credits", p.Credits), fmt.Sprintf("/person?id=%d", p.ID), "", p.Score})
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
	results, err := searchEpisodes(r.Context(), s.db, r.URL.Query().Get("q"), 30)
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
	var relPath string
	if err := s.db.QueryRow(`SELECT path FROM television_series WHERE id = ?`, id).Scan(&relPath); err != nil {
		http.Error(w, "series not found", 404)
		return
	}
	series, err := readTelevisionSeriesGz(filepath.Join(s.televisionDir, relPath))
	if err != nil {
		http.Error(w, "could not load series: "+err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages.ExecuteTemplate(w, "television.html", series); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func (s *server) handleAPIPeople(w http.ResponseWriter, r *http.Request) {
	results, err := searchPeople(r.Context(), s.db, r.URL.Query().Get("q"), 25)
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
			id = personIDByWiki(s.db, wiki)
		}
	}
	if id == 0 {
		if name := r.URL.Query().Get("name"); name != "" {
			id = personIDByName(s.db, name)
		}
	}
	if id == 0 {
		http.Error(w, "person not found", 404)
		return
	}
	fm, err := personFilmography(s.db, id)
	if err != nil {
		http.Error(w, "person not found", 404)
		return
	}
	fm.Image = fetchPersonImage(fm.Name) // live lookup; not stored
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages.ExecuteTemplate(w, "person.html", fm); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func (s *server) handleMovie(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.URL.Query().Get("id"))
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	var relPath string
	if err := s.db.QueryRow(`SELECT path FROM movies WHERE id = ?`, id).Scan(&relPath); err != nil {
		http.Error(w, "movie not found", 404)
		return
	}
	m, err := readMovieGz(filepath.Join(s.moviesDir, relPath))
	if err != nil {
		http.Error(w, "could not load movie: "+err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages.ExecuteTemplate(w, "movie.html", m); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

var funcs = template.FuncMap{
	"year": func(m *Movie) string {
		if len(m.ReleaseDates) > 0 && len(m.ReleaseDates[0]) >= 4 {
			return m.ReleaseDates[0][:4]
		}
		return ""
	},
	"join":            func(v []string) string { return strings.Join(v, ", ") },
	"title":           cleanTitle,
	"televisiontitle": cleanTelevisionTitle,
	"poster":          filePathURL,
	// people renders a []Person as comma-separated links to their person pages,
	// linking by Wiki target (→ Q-id identity) when present, else by name.
	"people": func(ps []Person) template.HTML {
		parts := make([]string, len(ps))
		for i, p := range ps {
			parts[i] = `<a class="plink" href="` + personHref(p) + `">` +
				template.HTMLEscapeString(p.Name) + `</a>`
		}
		return template.HTML(strings.Join(parts, ", "))
	},
	"phref": personHref,
}

// personHref builds the person-page URL for a credit: by wiki target (canonical
// identity) when linked, else by display name.
func personHref(p Person) string {
	if p.Wiki != "" {
		return "/person?wiki=" + url.QueryEscape(p.Wiki)
	}
	return "/person?name=" + url.QueryEscape(p.Name)
}

//go:embed templates/*.html
var templatesFS embed.FS

// pages holds all page templates, parsed from the embedded templates/ dir.
var pages = template.Must(template.New("").Funcs(funcs).ParseFS(templatesFS, "templates/*.html"))
