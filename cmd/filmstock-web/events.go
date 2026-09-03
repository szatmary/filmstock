package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/szatmary/filmstock/internal/query"
)

// handleAPIEvents serves the Events search mode.
func (s *server) handleAPIEvents(w http.ResponseWriter, r *http.Request) {
	res, err := query.SearchEvents(r.Context(), s.fs.SQL(), r.URL.Query().Get("q"), 25)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

// handleEventPage renders one ceremony or festival from its record.
func (s *server) handleEventPage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.URL.Query().Get("id"))
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	e, err := s.eventView(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "event not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "could not load event: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages.ExecuteTemplate(w, "event.html", e); err != nil {
		http.Error(w, err.Error(), 500)
	}
}
