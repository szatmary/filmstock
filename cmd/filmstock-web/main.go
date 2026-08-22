// Command filmstock-web is a sample application: a small web server built on
// nothing but the public github.com/szatmary/filmstock API.
//
// It exists to be run, and to prove the library is usable from outside. If a
// working server cannot be written against the exported surface, the exported
// surface is wrong — so this file deliberately imports no internals and reaches
// for no SQL of its own.
//
//	filmstock-web -db out/search.db -records out
//	filmstock-web -db out/search.db -remote https://github.com/.../releases/download/v2026-08-22
package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/szatmary/filmstock"
)

//go:embed page.html
var pageFS embed.FS

var page = template.Must(template.ParseFS(pageFS, "page.html"))

type server struct {
	db     *filmstock.DB
	remote bool
}

func main() {
	dbPath := flag.String("db", "out/search.db", "search database")
	records := flag.String("records", "", "local record tree (out/); mutually exclusive with -remote")
	remote := flag.String("remote", "", "base URL holding movies.pack / television.pack / events.pack")
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	// One or the other, never a silent default: which one is in use changes
	// whether opening a record costs a disk read or a network round trip, and
	// the operator should know which they got.
	var src filmstock.RecordSource
	switch {
	case *records != "" && *remote != "":
		log.Fatal("-records and -remote are mutually exclusive")
	case *records != "":
		src = filmstock.Dir(*records)
		log.Printf("records: local tree %s", *records)
	case *remote != "":
		src = filmstock.Remote(*remote)
		log.Printf("records: remote packs %s", *remote)
	default:
		log.Fatal("pass -records DIR or -remote URL")
	}

	db, err := filmstock.Open(*dbPath, src)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	s := &server{db: db, remote: *remote != ""}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.home)
	mux.HandleFunc("/api/search", s.search)
	mux.HandleFunc("/film", s.film)
	mux.HandleFunc("/series", s.series)

	log.Printf("listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func (s *server) home(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	page.Execute(w, map[string]any{"Remote": s.remote})
}

// search runs every path the library offers and merges them. All of this is
// database-only — no record is opened, so none of it touches the network even
// when -remote is in use.
func (s *server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if len(q) < 3 {
		writeJSON(w, []any{})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	type row struct {
		Kind, Title, Subtitle, Link string
		Score                       float64
	}
	out := []row{}

	films, err := s.db.SearchFilms(ctx, q, "title", 8)
	if err != nil {
		httpError(w, err)
		return
	}
	for _, f := range films {
		out = append(out, row{"film", f.Title, f.Director,
			fmt.Sprintf("/film?id=%d", f.ID), f.Score})
	}

	series, err := s.db.SearchSeries(ctx, q, "title", 8)
	if err != nil {
		httpError(w, err)
		return
	}
	for _, t := range series {
		out = append(out, row{"series", t.Title,
			fmt.Sprintf("%d seasons · %d episodes", t.SeasonsCount, t.EpisodesCount),
			fmt.Sprintf("/series?id=%d", t.ID), t.Score})
	}

	eps, err := s.db.SearchEpisodes(ctx, q, 5)
	if err != nil {
		httpError(w, err)
		return
	}
	for _, e := range eps {
		out = append(out, row{"episode", e.Title,
			fmt.Sprintf("%s · S%dE%d", e.SeriesTitle, e.Season, e.NumberInSeason),
			fmt.Sprintf("/series?id=%d", e.SeriesID), e.Score})
	}

	people, err := s.db.SearchPeople(ctx, q, 5)
	if err != nil {
		httpError(w, err)
		return
	}
	for _, p := range people {
		out = append(out, row{"person", p.Name,
			fmt.Sprintf("%d credits", p.Credits), "", p.Score})
	}
	writeJSON(w, out)
}

// film opens one full record. This is the only kind of request that costs a
// fetch: one file read, or one HTTP range request against movies.pack.
func (s *server) film(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.URL.Query().Get("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	started := time.Now()
	m, err := s.db.Film(r.Context(), id)
	if err != nil {
		httpError(w, err)
		return
	}
	w.Header().Set("X-Record-Fetch", time.Since(started).String())
	writeJSON(w, m)
}

func (s *server) series(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.URL.Query().Get("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	started := time.Now()
	t, err := s.db.Series(r.Context(), id)
	if err != nil {
		httpError(w, err)
		return
	}
	w.Header().Set("X-Record-Fetch", time.Since(started).String())
	writeJSON(w, t)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

// httpError reports the library's own message. Those messages are written to be
// actionable — "this database was not packed for remote use", not "not found" —
// so passing them through is more useful than flattening them to a status code.
func httpError(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
