// Command filmstock-web is the browser: search the database, open a record.
//
// It is built on the public github.com/szatmary/filmstock API and nothing else.
package main

import (
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
	_ "modernc.org/sqlite"
)

type server struct{ fs *filmstock.DB }

func main() {
	dbPath := flag.String("db", "index.db", "the index")
	records := flag.String("records", "filmstock-data", "record tree produced by `filmstock extract`")
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	fmt.Fprintf(os.Stderr, "records: %s\n", *records)
	db, err := filmstock.Open(*dbPath, filmstock.Dir(*records))
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	s := &server{fs: db}
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

	fmt.Fprintf(os.Stderr, "filmstock browser listening on http://localhost%s\n", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		fatal(err)
	}
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
	results, err := filmstock.SearchMovies(r.Context(), s.fs.SQL(), q, field, 25)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

func (s *server) handleAPITelevision(w http.ResponseWriter, r *http.Request) {
	field := r.URL.Query().Get("field")
	results, err := filmstock.SearchTelevision(r.Context(), s.fs.SQL(), r.URL.Query().Get("q"), field, 25)
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
	var mv []filmstock.SearchResult
	var television []filmstock.TelevisionSearchResult
	var ep []filmstock.EpisodeSearchResult
	var pe []filmstock.PersonResult
	var ev []filmstock.UnifiedResult
	var wg sync.WaitGroup
	wg.Add(5)
	go func() { defer wg.Done(); mv, _ = filmstock.SearchMovies(ctx, s.fs.SQL(), q, "title", 8) }()
	go func() { defer wg.Done(); television, _ = filmstock.SearchTelevision(ctx, s.fs.SQL(), q, "title", 8) }()
	go func() { defer wg.Done(); ep, _ = filmstock.SearchEpisodes(ctx, s.fs.SQL(), q, 6) }()
	go func() { defer wg.Done(); pe, _ = filmstock.SearchPeople(ctx, s.fs.SQL(), q, 6) }()
	go func() { defer wg.Done(); ev, _ = filmstock.SearchEvents(ctx, s.fs.SQL(), q, 4) }()
	wg.Wait()

	var out []filmstock.UnifiedResult
	yr := func(y int) string {
		if y > 0 {
			return fmt.Sprintf(" (%d)", y)
		}
		return ""
	}
	for _, m := range mv {
		out = append(out, filmstock.UnifiedResult{Type: "movie", Title: m.Title + yr(m.Year), Subtitle: m.Director, Link: fmt.Sprintf("/movie?id=%d", m.ID), Cover: m.Cover, Score: m.Score})
	}
	for _, t := range television {
		sub := fmt.Sprintf("%d seasons · %d episodes", t.SeasonsCount, t.EpisodesCount)
		if t.Network != "" {
			sub += " · " + t.Network
		}
		out = append(out, filmstock.UnifiedResult{Type: "television", Title: t.Title + yr(t.Year), Subtitle: sub, Link: fmt.Sprintf("/television?id=%d", t.ID), Cover: t.Cover, Score: t.Score})
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
		out = append(out, filmstock.UnifiedResult{Type: "episode", Title: e.Title, Subtitle: sub, Link: fmt.Sprintf("/television?id=%d", e.SeriesID), Cover: "", Score: e.Score})
		if epShown++; epShown >= 3 {
			break
		}
	}
	out = append(out, ev...)
	for _, p := range pe {
		out = append(out, filmstock.UnifiedResult{Type: "person", Title: p.Name, Subtitle: fmt.Sprintf("%d credits", p.Credits), Link: fmt.Sprintf("/person?id=%d", p.ID), Cover: "", Score: p.Score})
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
	results, err := filmstock.SearchEpisodes(r.Context(), s.fs.SQL(), r.URL.Query().Get("q"), 30)
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
	series, err := s.fs.Series(r.Context(), id)
	if errors.Is(err, filmstock.ErrNotFound) {
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
	results, err := filmstock.SearchPeople(r.Context(), s.fs.SQL(), r.URL.Query().Get("q"), 25)
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
			id = filmstock.PersonIDByWiki(s.fs.SQL(), wiki)
		}
	}
	if id == 0 {
		if name := r.URL.Query().Get("name"); name != "" {
			id = filmstock.PersonIDByName(s.fs.SQL(), name)
		}
	}
	if id == 0 {
		http.Error(w, "person not found", 404)
		return
	}
	fm, err := filmstock.PersonFilmography(s.fs.SQL(), id)
	if err != nil {
		http.Error(w, "person not found", 404)
		return
	}
	fm.Image = filmstock.FetchPersonImage(fm.Name) // live lookup; not stored
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
	m, err := s.fs.Film(r.Context(), id)
	if errors.Is(err, filmstock.ErrNotFound) {
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
	"year": func(m *filmstock.Movie) string {
		if len(m.ReleaseDates) > 0 && len(m.ReleaseDates[0]) >= 4 {
			return m.ReleaseDates[0][:4]
		}
		return ""
	},
	"join":            func(v []string) string { return strings.Join(v, ", ") },
	"title":           filmstock.CleanTitle,
	"televisiontitle": filmstock.CleanTelevisionTitle,
	"poster":          filmstock.FilePathURL,
	// people renders a []Person as comma-separated links to their person pages,
	// linking by Wiki target (→ Q-id identity) when present, else by name.
	"people": func(ps []filmstock.Person) template.HTML {
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
func personHref(p filmstock.Person) string {
	if p.Wiki != "" {
		return "/person?wiki=" + url.QueryEscape(p.Wiki)
	}
	return "/person?name=" + url.QueryEscape(p.Name)
}

//go:embed templates/*.html
var templatesFS embed.FS

// pages holds all page templates, parsed from the embedded templates/ dir.
var pages = template.Must(template.New("").Funcs(funcs).ParseFS(templatesFS, "templates/*.html"))
