package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/szatmary/filmstock"
)

// handleAPIEvents serves the Events search mode.
func (s *server) handleAPIEvents(w http.ResponseWriter, r *http.Request) {
	res, err := filmstock.SearchEvents(r.Context(), s.db, r.URL.Query().Get("q"), 25)
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
	var relPath string
	if err := s.db.QueryRow(`SELECT path FROM events WHERE id = ?`, id).Scan(&relPath); err != nil {
		http.Error(w, "event not found", 404)
		return
	}
	e, err := filmstock.ReadEventGz(filepath.Join(s.eventsDir, relPath))
	if err != nil {
		http.Error(w, "could not load event: "+err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages.ExecuteTemplate(w, "event.html", e); err != nil {
		http.Error(w, err.Error(), 500)
	}
}
