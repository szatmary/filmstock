package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The third stage of the cascade: a cross-encoder over the survivors.
//
// dense and ColBERT both precompute the document side, which is what makes them
// indexable and also what limits them — the document was encoded without ever
// having seen the query. A cross-encoder reads both together in one forward
// pass, so nothing can be precomputed and every candidate costs a full model
// invocation. That buys accuracy and forbids scale: it runs on ~50 passages, not
// 600,000.
//
// Go cannot run it, so it lives in embed/rerank.py and is addressed by passage
// ROW — the same integer the ColBERT store uses — which keeps the contract
// between the two processes to a list of ints.

type crossReranker struct {
	url string
}

// Score returns one score per row, in the order given.
func (c *crossReranker) Score(query string, rows []int) ([]float64, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	parts := make([]string, len(rows))
	for i, r := range rows {
		parts[i] = strconv.Itoa(r)
	}
	u := fmt.Sprintf("%s?q=%s&rows=%s", c.url, url.QueryEscape(query),
		url.QueryEscape(strings.Join(parts, ",")))
	cl := &http.Client{Timeout: 300 * time.Second}
	resp, err := cl.Get(u)
	if err != nil {
		return nil, fmt.Errorf("cross-encoder unreachable at %s: %w", c.url, err)
	}
	defer resp.Body.Close()
	var out struct {
		Model  string    `json:"model"`
		Scores []float64 `json:"scores"`
		Error  string    `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Error != "" {
		return nil, fmt.Errorf("cross-encoder: %s", out.Error)
	}
	if len(out.Scores) != len(rows) {
		return nil, fmt.Errorf("cross-encoder returned %d scores for %d rows",
			len(out.Scores), len(rows))
	}
	return out.Scores, nil
}

// rerankHits rescores the top k hits with the cross-encoder and rolls the result
// up to works, keeping each work's best passage.
//
// Only the head of the list is rescored; everything below k keeps its incoming
// order and sits underneath. That is deliberate — a cross-encoder pass over the
// tail costs far more than it can be worth once the list is already ordered by
// two cheaper models that agree.
func rerankHits(c *crossReranker, query string, hits []Hit, k, n int) ([]Hit, error) {
	if k > len(hits) {
		k = len(hits)
	}
	head := hits[:k]
	rows := make([]int, k)
	for i, h := range head {
		rows[i] = h.Passage
	}
	scores, err := c.Score(query, rows)
	if err != nil {
		return nil, err
	}
	rescored := make([]Hit, k)
	for i, h := range head {
		rescored[i] = Hit{PageID: h.PageID, Passage: h.Passage, Score: scores[i]}
	}
	sort.Slice(rescored, func(a, b int) bool { return rescored[a].Score > rescored[b].Score })

	best := map[int]Hit{}
	order := make([]Hit, 0, len(rescored))
	for _, h := range rescored {
		if cur, ok := best[h.PageID]; !ok || h.Score > cur.Score {
			best[h.PageID] = h
		}
	}
	for _, h := range rescored {
		if best[h.PageID].Passage == h.Passage {
			order = append(order, h)
		}
	}
	// Anything the cross-encoder never saw keeps its old order, below.
	seen := map[int]bool{}
	for _, h := range order {
		seen[h.PageID] = true
	}
	for _, h := range hits[k:] {
		if !seen[h.PageID] {
			seen[h.PageID] = true
			order = append(order, h)
		}
	}
	if len(order) > n {
		order = order[:n]
	}
	return order, nil
}
