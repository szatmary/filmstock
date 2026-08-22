package main

import "testing"

// A document found by BOTH retrievers must outrank one found by only a single
// retriever, even when the single-list document is ranked first there. Agreement
// across independent evidence is the entire value of fusing.
func TestFusionRewardsAgreement(t *testing.T) {
	lex := rankedList{ids: []int{100, 200, 300}}
	sem := rankedList{ids: []int{300, 400, 500}}
	got := fuseRRF([]rankedList{lex, sem}, nil, rrfK)

	if got[0].PageID != 300 {
		t.Fatalf("want 300 first (found by both), got %d", got[0].PageID)
	}
	if got[0].LexRank != 3 || got[0].SemRank != 1 {
		t.Errorf("provenance wrong: lex=%d sem=%d, want 3 and 1", got[0].LexRank, got[0].SemRank)
	}
}

// Documents only one retriever found must still appear. Semantic search cannot
// answer "Jaws" and lexical search cannot answer "shark terrorises a beach town";
// dropping either list's exclusives would lose exactly what fusion is for.
func TestFusionKeepsExclusives(t *testing.T) {
	lex := rankedList{ids: []int{10, 11}}
	sem := rankedList{ids: []int{20, 21}}
	got := fuseRRF([]rankedList{lex, sem}, nil, rrfK)
	seen := map[int]bool{}
	for _, h := range got {
		seen[h.PageID] = true
	}
	for _, id := range []int{10, 11, 20, 21} {
		if !seen[id] {
			t.Errorf("page_id %d was dropped", id)
		}
	}
}

// Absence from a list must not be a penalty: a retriever returning its top N
// says nothing about documents past the cut-off.
func TestAbsenceIsNotAPenalty(t *testing.T) {
	// 10 is rank 1 lexically and absent semantically; 99 is rank 2 in both.
	lex := rankedList{ids: []int{10, 99}}
	sem := rankedList{ids: []int{50, 99}}
	got := fuseRRF([]rankedList{lex, sem}, nil, rrfK)

	var s10, s99 float64
	for _, h := range got {
		switch h.PageID {
		case 10:
			s10 = h.Score
		case 99:
			s99 = h.Score
		}
	}
	if s99 <= s10 {
		t.Errorf("99 (rank 2 in both lists) scored %.5f, not above 10 (rank 1 in one) %.5f", s99, s10)
	}
}

// Weighting must actually shift the outcome, so the balance is tunable once the
// eval harness can measure which retriever deserves more say.
func TestWeightsShiftTheResult(t *testing.T) {
	lex := rankedList{ids: []int{1}}
	sem := rankedList{ids: []int{2}}

	even := fuseRRF([]rankedList{lex, sem}, nil, rrfK)
	if even[0].PageID != 1 { // tie broken by page_id
		t.Fatalf("equal weights should tie and break by id, got %d", even[0].PageID)
	}
	semHeavy := fuseRRF([]rankedList{lex, sem}, []float64{1, 5}, rrfK)
	if semHeavy[0].PageID != 2 {
		t.Errorf("with semantic weighted 5x, want 2 first, got %d", semHeavy[0].PageID)
	}
}

// Ordering must not depend on map iteration order, or results change run to run.
func TestFusionIsDeterministic(t *testing.T) {
	lex := rankedList{ids: []int{5, 6, 7, 8}}
	sem := rankedList{ids: []int{8, 7, 6, 5}}
	first := fuseRRF([]rankedList{lex, sem}, nil, rrfK)
	for i := 0; i < 20; i++ {
		again := fuseRRF([]rankedList{lex, sem}, nil, rrfK)
		for j := range first {
			if first[j].PageID != again[j].PageID {
				t.Fatalf("ordering varies between runs at position %d", j)
			}
		}
	}
}

func TestFusionHandlesEmptyLists(t *testing.T) {
	only := fuseRRF([]rankedList{{ids: []int{1, 2}}, {}}, nil, rrfK)
	if len(only) != 2 || only[0].PageID != 1 {
		t.Errorf("a single non-empty list should pass through in order, got %+v", only)
	}
	if got := fuseRRF([]rankedList{{}, {}}, nil, rrfK); len(got) != 0 {
		t.Errorf("two empty lists should fuse to nothing, got %d", len(got))
	}
}
