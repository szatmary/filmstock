package main

import (
	"sort"

	"github.com/szatmary/filmstock"
)

// Reciprocal Rank Fusion of the lexical and semantic result lists.
//
//	score(d) = sum over lists of  1 / (k + rank_i(d))
//
// RRF combines RANKS, not scores, which is the whole reason to use it here: the
// two retrievers produce numbers that are not comparable in any meaningful way.
// Trigram search returns a Sørensen-Dice overlap in [0,1]; the vector cascade
// returns an int8 dot product whose magnitude depends on per-dimension
// calibration scales. Normalising those onto a common range would be inventing a
// relationship that does not exist, and would drift the moment either retriever
// is retuned. Ranks are directly comparable by construction.
//
// The two are kept because they fail in opposite directions. Lexical search wins
// exact titles ("Jaws") and proper nouns, and is hopeless on paraphrase — it
// scored 0% on the concept query set. Semantic search wins paraphrase and is
// weaker on exact identifiers, since an embedding of "Jaws" is not especially
// close to the article about it. Neither replaces the other.
const rrfK = 60 // the standard constant; large enough that rank 1 does not dominate

// FusedHit is one work with its fused score and where each retriever placed it,
// which is what makes a bad ranking debuggable.
type FusedHit struct {
	PageID   int
	Score    float64
	LexRank  int // 0 = absent from that list
	SemRank  int
	LexScore float64
	SemScore float64
}

// rankedList is a retriever's output: page_ids in rank order, best first.
type rankedList struct {
	ids    []int
	scores []float64
}

// fuseRRF merges any number of ranked lists. Weights let one retriever count for
// more than another; nil means equal.
//
// A document missing from a list contributes nothing from it rather than being
// penalised — that is deliberate. Semantic search returns only its top N, so
// "absent" means "not retrieved", not "judged irrelevant", and treating it as a
// negative would punish every document outside the cut-off.
func fuseRRF(lists []rankedList, weights []float64, k float64) []FusedHit {
	if k <= 0 {
		k = rrfK
	}
	agg := map[int]*FusedHit{}
	for li, l := range lists {
		w := 1.0
		if weights != nil && li < len(weights) {
			w = weights[li]
		}
		for r, id := range l.ids {
			h := agg[id]
			if h == nil {
				h = &FusedHit{PageID: id}
				agg[id] = h
			}
			h.Score += w / (k + float64(r+1))
			switch li {
			case 0:
				h.LexRank = r + 1
				if r < len(l.scores) {
					h.LexScore = l.scores[r]
				}
			case 1:
				h.SemRank = r + 1
				if r < len(l.scores) {
					h.SemScore = l.scores[r]
				}
			}
		}
	}
	out := make([]FusedHit, 0, len(agg))
	for _, h := range agg {
		out = append(out, *h)
	}
	// Ties broken by page_id so the ordering is deterministic — otherwise map
	// iteration order leaks into results and they change between runs.
	sort.Slice(out, func(a, b int) bool {
		if out[a].Score != out[b].Score {
			return out[a].Score > out[b].Score
		}
		return out[a].PageID < out[b].PageID
	})
	return out
}

// hitsToList converts the vector cascade's output into a ranked list.
func hitsToList(hits []Hit) rankedList {
	l := rankedList{ids: make([]int, len(hits)), scores: make([]float64, len(hits))}
	for i, h := range hits {
		l.ids[i] = h.PageID
		l.scores[i] = h.Score
	}
	return l
}

// resultsToList converts lexical search results into a ranked list.
func resultsToList(res []filmstock.SearchResult) rankedList {
	l := rankedList{ids: make([]int, len(res)), scores: make([]float64, len(res))}
	for i, r := range res {
		l.ids[i] = r.ID
		l.scores[i] = r.Score
	}
	return l
}
