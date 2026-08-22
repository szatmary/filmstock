package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"sync"
)

// The on-device vector search: a 2-bit scan over everything, then an int8 rerank
// of the survivors.
//
//	int2 scan   543,200 x 1024 dims = 139 MB resident, touched in full per query
//	int8 rerank top ~1000 rows only = ~1 MB of mmap'd random reads
//	roll up      passages -> works, taking each work's best passage
//
// Neither stage multiplies by a per-dimension scale. The int2 reconstruction
// levels are s*(2c-3)/3, so
//
//	score = sum_d s[d]*(2*code_d-3)/3 * q[d] = sum_d (2*code_d-3) * w[d]
//
// with w[d] = s[d]*q[d]/3 folded once per query.
//
// The inner loop then uses a lookup table rather than arithmetic. Four codes are
// packed per byte, so for each group of four dimensions we precompute the sum for
// all 256 possible byte values:
//
//	lut[g][b] = sum_{j<4} (2*((b>>2j)&3) - 3) * w[4g+j]
//
// A vector's score becomes 256 table lookups instead of 1024 multiply-adds — a
// 4x cut in inner-loop work, and no multiplication at all in the hot path, which
// is what makes this viable on a Pi 4's four Cortex-A72 cores.

type quantIndex struct {
	Dim     int
	Count   int
	Scales8 []float32 // int8 calibration (wide, p99.9)
	Scales2 []float32 // int2 calibration (1.494 sigma)

	int2 []byte // Count x ceil(Dim/4)
	int8 []byte // Count x Dim
	ids  []int32

	int2Stride int
}

// Hit is one scored work (not passage): passages have already been rolled up.
type Hit struct {
	PageID  int
	Score   float64
	Passage int // index of the best-scoring passage
}

func loadQuantIndex(manifestPath, idsPath string) (*quantIndex, error) {
	var man quantManifest
	b, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &man); err != nil {
		return nil, err
	}
	ix := &quantIndex{
		Dim: man.Dim, Count: man.Count,
		Scales8: man.Scales, Scales2: man.Scales2,
		int2Stride: (man.Dim + 3) / 4,
	}
	if ix.int2, err = os.ReadFile(man.Int2Path); err != nil {
		return nil, err
	}
	if ix.int8, err = os.ReadFile(man.Int8Path); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(idsPath)
	if err != nil {
		return nil, err
	}
	ix.ids = make([]int32, len(raw)/4)
	for i := range ix.ids {
		ix.ids[i] = int32(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	if len(ix.ids) < ix.Count {
		return nil, fmt.Errorf("passages.bin has %d ids but index has %d rows", len(ix.ids), ix.Count)
	}
	return ix, nil
}

// buildLUT precomputes the per-byte partial sums for one query.
func (ix *quantIndex) buildLUT(q []float32) []float32 {
	groups := ix.int2Stride
	lut := make([]float32, groups*256)
	for g := 0; g < groups; g++ {
		base := g * 256
		for b := 0; b < 256; b++ {
			var sum float32
			for j := 0; j < 4; j++ {
				d := g*4 + j
				if d >= ix.Dim {
					break
				}
				c := (b >> uint(2*j)) & 3
				w := ix.Scales2[d] * q[d] / 3
				sum += float32(2*c-3) * w
			}
			lut[base+b] = sum
		}
	}
	return lut
}

// scanInt2 scores every vector and returns the top k row indices.
func (ix *quantIndex) scanInt2(q []float32, k int) []int {
	lut := ix.buildLUT(q)
	stride := ix.int2Stride
	workers := runtime.NumCPU()
	type part struct {
		idx []int
		sc  []float32
	}
	parts := make([]part, workers)

	var wg sync.WaitGroup
	chunk := (ix.Count + workers - 1) / workers
	for w := 0; w < workers; w++ {
		lo := w * chunk
		hi := lo + chunk
		if hi > ix.Count {
			hi = ix.Count
		}
		if lo >= hi {
			continue
		}
		wg.Add(1)
		go func(w, lo, hi int) {
			defer wg.Done()
			top := newTopK(k)
			for i := lo; i < hi; i++ {
				row := ix.int2[i*stride : (i+1)*stride]
				var s float32
				for g := 0; g < stride; g++ {
					s += lut[g*256+int(row[g])]
				}
				top.push(i, s)
			}
			parts[w].idx, parts[w].sc = top.drain()
		}(w, lo, hi)
	}
	wg.Wait()

	merged := newTopK(k)
	for _, p := range parts {
		for i := range p.idx {
			merged.push(p.idx[i], p.sc[i])
		}
	}
	idx, _ := merged.drain()
	return idx
}

// rerankInt8 rescores candidate rows at int8 precision. Only these rows are
// touched, so this stage costs ~1 MB of reads regardless of corpus size.
func (ix *quantIndex) rerankInt8(q []float32, cand []int) []Hit {
	w := make([]float32, ix.Dim)
	for d := range w {
		w[d] = ix.Scales8[d] * q[d] / 127
	}
	out := make([]Hit, 0, len(cand))
	for _, i := range cand {
		row := ix.int8[i*ix.Dim : (i+1)*ix.Dim]
		var s float32
		for d := 0; d < ix.Dim; d++ {
			s += float32(int8(row[d])) * w[d]
		}
		out = append(out, Hit{PageID: int(ix.ids[i]), Score: float64(s), Passage: i})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Score > out[b].Score })
	return out
}

// Search runs the full cascade and rolls passages up to works.
//
// Roll-up takes each work's BEST passage rather than summing: a film should rank
// on its strongest match, not on having many mediocre ones. Summing would favour
// long articles purely for being long.
func (ix *quantIndex) Search(q []float32, coarseK, finalN int) []Hit {
	if coarseK < finalN {
		coarseK = finalN * 20
	}
	cand := ix.scanInt2(q, coarseK)
	scored := ix.rerankInt8(q, cand)

	best := map[int]Hit{}
	for _, h := range scored {
		if cur, ok := best[h.PageID]; !ok || h.Score > cur.Score {
			best[h.PageID] = h
		}
	}
	out := make([]Hit, 0, len(best))
	for _, h := range best {
		out = append(out, h)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Score > out[b].Score })
	if len(out) > finalN {
		out = out[:finalN]
	}
	return out
}

// exactFloat32 is the ground truth used to measure what the cascade loses. Never
// used at serving time — float32 vectors do not ship.
func exactFloat32(vecs []float32, dim, count int, q []float32, n int) []int {
	type sc struct {
		i int
		s float32
	}
	all := make([]sc, count)
	for i := 0; i < count; i++ {
		var s float32
		row := vecs[i*dim : (i+1)*dim]
		for d := 0; d < dim; d++ {
			s += row[d] * q[d]
		}
		all[i] = sc{i, s}
	}
	sort.Slice(all, func(a, b int) bool { return all[a].s > all[b].s })
	out := make([]int, 0, n)
	for i := 0; i < n && i < len(all); i++ {
		out = append(out, all[i].i)
	}
	return out
}

// topK keeps the k highest scores seen, using a min-heap so the scan never sorts
// the whole corpus.
type topK struct {
	k   int
	idx []int
	sc  []float32
}

func newTopK(k int) *topK {
	return &topK{k: k, idx: make([]int, 0, k+1), sc: make([]float32, 0, k+1)}
}

func (t *topK) push(i int, s float32) {
	if len(t.idx) < t.k {
		t.idx = append(t.idx, i)
		t.sc = append(t.sc, s)
		t.up(len(t.idx) - 1)
		return
	}
	if s <= t.sc[0] {
		return
	}
	t.idx[0], t.sc[0] = i, s
	t.down(0)
}

func (t *topK) up(i int) {
	for i > 0 {
		p := (i - 1) / 2
		if t.sc[p] <= t.sc[i] {
			break
		}
		t.idx[p], t.idx[i] = t.idx[i], t.idx[p]
		t.sc[p], t.sc[i] = t.sc[i], t.sc[p]
		i = p
	}
}

func (t *topK) down(i int) {
	n := len(t.sc)
	for {
		l, r, m := 2*i+1, 2*i+2, i
		if l < n && t.sc[l] < t.sc[m] {
			m = l
		}
		if r < n && t.sc[r] < t.sc[m] {
			m = r
		}
		if m == i {
			return
		}
		t.idx[m], t.idx[i] = t.idx[i], t.idx[m]
		t.sc[m], t.sc[i] = t.sc[i], t.sc[m]
		i = m
	}
}

func (t *topK) drain() ([]int, []float32) {
	idx := append([]int(nil), t.idx...)
	sc := append([]float32(nil), t.sc...)
	sort.Sort(&byScoreDesc{idx, sc})
	return idx, sc
}

type byScoreDesc struct {
	idx []int
	sc  []float32
}

func (b *byScoreDesc) Len() int           { return len(b.idx) }
func (b *byScoreDesc) Less(i, j int) bool { return b.sc[i] > b.sc[j] }
func (b *byScoreDesc) Swap(i, j int) {
	b.idx[i], b.idx[j] = b.idx[j], b.idx[i]
	b.sc[i], b.sc[j] = b.sc[j], b.sc[i]
}

var _ = math.Sqrt
