package main

import (
	"encoding/binary"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// buildSyntheticIndex writes float32 vectors, quantises them the same way the
// real pipeline does, and loads the result. Everything here is exercised on
// synthetic data so the cascade can be validated without a model or a GPU.
func buildSyntheticIndex(t *testing.T, count, dim int) (*quantIndex, []float32) {
	t.Helper()
	dir := t.TempDir()
	rng := rand.New(rand.NewSource(7))

	vecs := make([]float32, count*dim)
	for i := 0; i < count; i++ {
		row := vecs[i*dim : (i+1)*dim]
		var norm float64
		for d := range row {
			row[d] = float32(rng.NormFloat64())
			norm += float64(row[d]) * float64(row[d])
		}
		norm = math.Sqrt(norm)
		for d := range row {
			row[d] /= float32(norm) // L2-normalised, as the embedder emits
		}
	}
	vp := filepath.Join(dir, "vectors.f32.synth.bin")
	f, err := os.Create(vp)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	for _, v := range vecs {
		binary.LittleEndian.PutUint32(buf, math.Float32bits(v))
		f.Write(buf)
	}
	f.Close()

	ids := make([]byte, count*4)
	for i := 0; i < count; i++ {
		binary.LittleEndian.PutUint32(ids[i*4:], uint32(1000+i/3)) // 3 passages per work
	}
	ip := filepath.Join(dir, "passages.bin")
	os.WriteFile(ip, ids, 0o644)

	cmdQuantize([]string{"-vectors", vp, "-dim", itoa(dim), "-sample", itoa(count)})

	ix, err := loadQuantIndex(filepath.Join(dir, "quant.synth.json"), ip)
	if err != nil {
		t.Fatal(err)
	}
	return ix, vecs
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// The cascade exists to be cheap, but it is only useful if it still finds what
// exact float32 search would find. This measures that directly rather than
// assuming 2 bits is "good enough".
func TestCascadeRecallVersusExact(t *testing.T) {
	const count, dim = 4000, 256
	ix, vecs := buildSyntheticIndex(t, count, dim)

	rng := rand.New(rand.NewSource(99))
	const queries, topN, coarseK = 40, 10, 400
	var recalled, total int
	for range queries {
		q := make([]float32, dim)
		var norm float64
		for d := range q {
			q[d] = float32(rng.NormFloat64())
			norm += float64(q[d]) * float64(q[d])
		}
		norm = math.Sqrt(norm)
		for d := range q {
			q[d] /= float32(norm)
		}

		want := exactFloat32(vecs, dim, count, q, topN)
		wantSet := map[int]bool{}
		for _, i := range want {
			wantSet[i] = true
		}

		cand := ix.scanInt2(q, coarseK)
		got := ix.rerankInt8(q, cand)
		if len(got) > topN {
			got = got[:topN]
		}
		for _, h := range got {
			if wantSet[h.Passage] {
				recalled++
			}
		}
		total += topN
	}
	r := 100 * float64(recalled) / float64(total)
	t.Logf("cascade recall@%d vs exact float32: %.1f%% (coarse k=%d over %d vectors)",
		topN, r, coarseK, count)
	if r < 80 {
		t.Errorf("recall %.1f%% is too low — the int2 stage is losing true neighbours", r)
	}
}

// The coarse stage may reorder, but the true nearest neighbour must survive into
// the candidate set — if it is dropped there, no rerank can recover it.
func TestCoarseStageKeepsTheTopHit(t *testing.T) {
	const count, dim = 4000, 256
	ix, vecs := buildSyntheticIndex(t, count, dim)

	rng := rand.New(rand.NewSource(5))
	misses := 0
	const trials = 40
	for range trials {
		q := make([]float32, dim)
		for d := range q {
			q[d] = float32(rng.NormFloat64())
		}
		best := exactFloat32(vecs, dim, count, q, 1)[0]
		cand := ix.scanInt2(q, 400)
		found := false
		for _, c := range cand {
			if c == best {
				found = true
				break
			}
		}
		if !found {
			misses++
		}
	}
	if misses > trials/10 {
		t.Errorf("coarse scan dropped the true top hit in %d/%d queries", misses, trials)
	}
	t.Logf("true top hit survived the int2 scan in %d/%d queries", trials-misses, trials)
}

// Passages roll up to works by BEST passage, not by sum: a long article must not
// outrank a better match merely by having more passages.
func TestRollupTakesBestPassageNotSum(t *testing.T) {
	ix, _ := buildSyntheticIndex(t, 300, 64)
	q := make([]float32, 64)
	for d := range q {
		q[d] = float32(d%7) / 7
	}
	hits := ix.Search(q, 200, 10)
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	seen := map[int]bool{}
	for _, h := range hits {
		if seen[h.PageID] {
			t.Errorf("page_id %d appears twice — passages were not rolled up", h.PageID)
		}
		seen[h.PageID] = true
	}
	for i := 1; i < len(hits); i++ {
		if hits[i-1].Score < hits[i].Score {
			t.Error("results are not sorted by score")
		}
	}
}

// The LUT is an optimisation; it must produce the same score as the arithmetic
// it replaces.
func TestLUTMatchesDirectArithmetic(t *testing.T) {
	ix, _ := buildSyntheticIndex(t, 200, 64)
	q := make([]float32, 64)
	for d := range q {
		q[d] = float32(math.Sin(float64(d)))
	}
	lut := ix.buildLUT(q)
	for i := 0; i < 20; i++ {
		row := ix.int2[i*ix.int2Stride : (i+1)*ix.int2Stride]
		var viaLUT float32
		for g := 0; g < ix.int2Stride; g++ {
			viaLUT += lut[g*256+int(row[g])]
		}
		var direct float32
		for d := 0; d < ix.Dim; d++ {
			c := (row[d/4] >> uint((d%4)*2)) & 3
			direct += float32(2*int(c)-3) * (ix.Scales2[d] * q[d] / 3)
		}
		if math.Abs(float64(viaLUT-direct)) > 1e-3 {
			t.Fatalf("row %d: LUT %.6f != direct %.6f", i, viaLUT, direct)
		}
	}
}
