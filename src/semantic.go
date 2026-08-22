package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// Semantic search for the browser.
//
// The split is forced by what each side can do: Go cannot run a transformer, but
// it owns the int2 -> int8 cascade and the record store. So Python embeds ONE
// short query per search (embed/testui.py, /api/embed) and Go does everything
// else. That keeps the shipping search path — quantised scan, rerank, roll-up —
// exercised by the real UI rather than only by tests.
//
// The model and prefix used for the query MUST match the ones that produced the
// document vectors. That is enforced on the Python side, where both live in one
// table, because a mismatch produces normal-looking vectors that merely rank
// worse, with no error anywhere.
type semanticSearcher struct {
	embedURL string
	quant    string
	ids      string

	once sync.Once
	ix   *quantIndex
	err  error
}

func newSemanticSearcher(embedURL, quantPath, idsPath string) *semanticSearcher {
	return &semanticSearcher{embedURL: embedURL, quant: quantPath, ids: idsPath}
}

// ready loads the quantised index on first use. It is ~700 MB, so loading it
// lazily keeps the server startable without the vector artifacts present.
func (s *semanticSearcher) ready() error {
	s.once.Do(func() {
		if s.quant == "" {
			s.err = fmt.Errorf("semantic search disabled: -quant not set")
			return
		}
		s.ix, s.err = loadQuantIndex(s.quant, s.ids)
	})
	return s.err
}

// embedQuery asks the sidecar for a query vector.
func (s *semanticSearcher) embedQuery(q string) ([]float32, error) {
	u := s.embedURL + "?q=" + url.QueryEscape(q)
	cl := &http.Client{Timeout: 120 * time.Second}
	resp, err := cl.Get(u)
	if err != nil {
		return nil, fmt.Errorf("query embedder unreachable at %s: %w", s.embedURL, err)
	}
	defer resp.Body.Close()
	var out struct {
		Dim    int       `json:"dim"`
		Vector []float32 `json:"vector"`
		Error  string    `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Error != "" {
		return nil, fmt.Errorf("query embedder: %s", out.Error)
	}
	if len(out.Vector) == 0 {
		return nil, fmt.Errorf("query embedder returned no vector")
	}
	return out.Vector, nil
}

// handleAPISemantic returns works ranked by semantic similarity.
func (s *server) handleAPISemantic(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]unifiedResult{})
		return
	}
	n := 25
	if v, err := strconv.Atoi(r.URL.Query().Get("n")); err == nil && v > 0 && v <= 100 {
		n = v
	}
	if s.sem == nil {
		http.Error(w, "semantic search not configured (-quant)", 503)
		return
	}
	if err := s.sem.ready(); err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	qv, err := s.sem.embedQuery(q)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	if len(qv) != s.sem.ix.Dim {
		http.Error(w, fmt.Sprintf(
			"dimension mismatch: query %d, index %d — the embedder is running a different model than the one that built the vectors",
			len(qv), s.sem.ix.Dim), 500)
		return
	}

	hits := s.sem.ix.Search(qv, 4000, n)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.hitsToResults(hits))
}

// hitsToResults turns scored page_ids into browser rows. Shared by both vector
// paths so a hit renders identically however it was found.
func (s *server) hitsToResults(hits []Hit) []unifiedResult {
	out := make([]unifiedResult, 0, len(hits))
	for _, h := range hits {
		var title, cover string
		var year interface{}
		err := s.db.QueryRow(
			`SELECT title, year, coalesce(cover_image_file,'') FROM movies WHERE id=?`, h.PageID,
		).Scan(&title, &year, &cover)
		if err != nil {
			continue
		}
		yr := ""
		if year != nil {
			yr = fmt.Sprint(year)
		}
		out = append(out, unifiedResult{
			Type:     "movie",
			Title:    cleanTitle(title),
			Subtitle: yr,
			Link:     fmt.Sprintf("/movie?id=%d", h.PageID),
			Cover:    filePathURL(cover, 160),
			Score:    h.Score,
		})
	}
	return out
}
