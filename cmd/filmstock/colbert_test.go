package main

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

// Checks the Go reader against float32 MaxSim computed by the encoder itself.
//
// Everything this exercises fails silently: an off-by-one in the offset table, a
// dropped punctuation token, a scale applied in the wrong place or the wrong
// order. None of them error — they just rank slightly worse, which is
// indistinguishable from "the model is mediocre" without a reference.
//
// Opt-in because it needs artifacts:
//
//	venv/bin/python embed/colbert.py encode --limit 2000 --out DIR
//	venv/bin/python embed/colbert_ref.py --out DIR --ref DIR/ref.json
//	MEDIADB_COLBERT_MANIFEST=DIR/colbert.<tag>.json \
//	MEDIADB_COLBERT_REF=DIR/ref.json go test -run Colbert -v
type colbertRef struct {
	Query  string      `json:"query"`
	Dim    int         `json:"dim"`
	Tokens [][]float32 `json:"query_tokens"`
	Rows   []int       `json:"rows"`
	Scores []float64   `json:"scores"` // float32 MaxSim, before quantisation
}

func TestColbertMaxSimMatchesFloat32(t *testing.T) {
	manPath := os.Getenv("MEDIADB_COLBERT_MANIFEST")
	refPath := os.Getenv("MEDIADB_COLBERT_REF")
	if manPath == "" || refPath == "" {
		t.Skip("set MEDIADB_COLBERT_MANIFEST and MEDIADB_COLBERT_REF (see colbert_ref.py)")
	}
	ix, err := loadColbertIndex(manPath)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(refPath)
	if err != nil {
		t.Fatal(err)
	}
	var ref colbertRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatal(err)
	}
	if ref.Dim != ix.Dim {
		t.Fatalf("reference dim %d, index dim %d", ref.Dim, ix.Dim)
	}

	got := ix.Rerank(ref.Tokens, ref.Rows)
	byRow := map[int]float64{}
	for _, h := range got {
		byRow[h.Passage] = h.Score
	}
	// int8 at p99.9 per dimension keeps ~4 significant bits per component; the
	// dense path measures 0.9999 cosine at the same setting, so anything above a
	// percent here is a bug, not quantisation.
	const tol = 0.01
	for i, row := range ref.Rows {
		want := ref.Scores[i]
		have := byRow[row]
		if math.Abs(want) < 1e-9 {
			continue
		}
		if rel := math.Abs(have-want) / math.Abs(want); rel > tol {
			t.Errorf("row %d: MaxSim %.4f, float32 reference %.4f (%.2f%% off)",
				row, have, want, 100*rel)
		}
	}

	// Ranking is what actually ships: the scores may drift, the order must not.
	for i := 1; i < len(got); i++ {
		if got[i-1].Score < got[i].Score {
			t.Fatalf("Rerank returned unsorted output at %d", i)
		}
	}
}
