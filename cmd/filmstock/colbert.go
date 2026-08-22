package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/szatmary/filmstock"
)

// Late interaction: the second search path.
//
// The dense path (vecsearch.go) compresses a passage to ONE vector, which is
// what makes it scannable on a Pi and is also its ceiling — every term in the
// query is answered by the same averaged direction. ColBERT keeps one vector per
// token and scores
//
//	MaxSim(q, d) = sum_i max_j  q_i . d_j
//
// so each query term independently finds the evidence that supports it. "1978
// box office flops" can match the year in one token and the bomb in another,
// twenty words apart, which a single averaged vector cannot represent.
//
// It is used here as a RERANKER, not a retriever. Full ColBERT retrieval needs a
// centroid/PLAID index over ~120M token vectors; reranking the dense path's
// candidates needs no new structure at all and touches only the candidates:
//
//	int2 scan  -> 4000 passages     (dense, whole corpus, RAM-resident)
//	int8 rerank-> 4000 rescored     (dense)
//	MaxSim     -> top ~500 rescored (this file, ~10 MB of mmap'd random reads)
//
// The cost of that choice is explicit: recall is capped by what the dense stage
// retrieved, so eval reports the dense path beside it rather than only the
// combined number.
//
// The store is per-dimension quantised (int8, or int4/int2 via colbert-quantize)
// and the dense path's trick applies — folding the scale into the query once per
// query leaves an inner loop of a small integer times a float.
//
// MEASURED (2026-08-13): below int8 the ranking collapses (MRR 0.697 -> 0.456 at
// int4 -> 0.073 at int2) even though the vectors survive to 0.996 cosine. MaxSim
// scores span ~1% between passages, so a 1-2% per-passage error is larger than
// the entire signal. -center (quantise the residual from the mean token vector)
// exists to widen that margin; anything below int8 must be re-scored, never
// assumed.
//
// Token vectors are mmap'd rather than read: the blob is far larger than the
// working set of any one query, and the page cache is better at deciding what
// stays resident than we are.

type colbertManifest struct {
	Model        string    `json:"model"`
	Bits         int       `json:"bits"`
	Dim          int       `json:"dim"`
	Count        int       `json:"count"`
	TotalTokens  int64     `json:"total_tokens"`
	MeanTokens   float64   `json:"mean_tokens"`
	DocMaxlen    int       `json:"doc_maxlen"`
	QueryMaxlen  int       `json:"query_maxlen"`
	Scales       []float32 `json:"scales_int8"`
	TokensPath   string    `json:"tokens_path"`
	OffsetsPath  string    `json:"offsets_path"`
	LensPath     string    `json:"lens_path"`
	PageIDsPath  string    `json:"pageids_path"`
	TokensBytes  int64     `json:"tokens_bytes"`
	PassagesFrom string    `json:"passages_source"`

	// set by colbert-quantize only
	Mean          []float32 `json:"mean_token,omitempty"`
	CosineToInt8  float64   `json:"mean_cosine_to_int8,omitempty"`
	ElapsedSecond float64   `json:"requantise_s,omitempty"`
}

// scoreOpts are the ranking knobs measured against the 0.575 baseline. They are
// all query-side or scoring-side: none requires re-encoding the 13 GB store.
type scoreOpts struct {
	RealTokens int     // score over the first N query tokens only (0 = all 32)
	LenNorm    float64 // subtract LenNorm*log(doc tokens) — longer passages get
	// more chances to win a max, which favours long articles
	Notability float64 // + Notability*log(1+passages for this work)
}

type colbertIndex struct {
	Dim   int
	Count int
	Bits  int
	Model string

	scales  []float32
	nb      map[int]float64 // page_id -> log(1+passages), lazily built
	mean    []float32       // subtracted before quantising; nil unless -center was used
	offsets []uint64        // token index where each passage's rows start
	lens    []uint16        // token count per passage
	ids     []int32         // passage -> page_id
	tokens  []byte          // mmap: total_tokens x stride
	stride  int             // packed bytes per token vector
}

func loadColbertIndex(manifestPath string) (*colbertIndex, error) {
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}
	var man colbertManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		return nil, err
	}
	if len(man.Scales) != man.Dim {
		return nil, fmt.Errorf("%s: %d scales for %d dims", manifestPath, len(man.Scales), man.Dim)
	}
	if colbertStride(man.Dim, man.Bits) == 0 {
		return nil, fmt.Errorf("%s: bits=%d, expected 8, 4 or 2", manifestPath, man.Bits)
	}
	if man.Mean != nil && len(man.Mean) != man.Dim {
		return nil, fmt.Errorf("%s: mean_token has %d dims, index has %d",
			manifestPath, len(man.Mean), man.Dim)
	}
	ix := &colbertIndex{
		Dim: man.Dim, Count: man.Count, Bits: man.Bits, Model: man.Model,
		scales: man.Scales, mean: man.Mean, stride: colbertStride(man.Dim, man.Bits),
	}

	if ix.offsets, err = readU64(man.OffsetsPath, man.Count); err != nil {
		return nil, err
	}
	if ix.lens, err = readU16(man.LensPath, man.Count); err != nil {
		return nil, err
	}
	if ix.ids, err = readI32(man.PageIDsPath, man.Count); err != nil {
		return nil, err
	}

	f, err := os.Open(man.TokensPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	want := man.TotalTokens * int64(ix.stride)
	if st.Size() != want {
		return nil, fmt.Errorf("%s is %d bytes, manifest says %d tokens x %d bytes (%d dims "+
			"at %d bits) = %d — the blob and the manifest are from different runs",
			man.TokensPath, st.Size(), man.TotalTokens, ix.stride, man.Dim, man.Bits, want)
	}
	ix.tokens, err = syscall.Mmap(int(f.Fd()), 0, int(st.Size()),
		syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("mmap %s: %w", man.TokensPath, err)
	}
	// Random access by design — readahead would fault in megabytes per candidate.
	_ = syscall.Madvise(ix.tokens, syscall.MADV_RANDOM)
	return ix, nil
}

func readU64(path string, n int) ([]uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) != n*8 {
		return nil, fmt.Errorf("%s: %d bytes, expected %d", path, len(b), n*8)
	}
	out := make([]uint64, n)
	for i := range out {
		out[i] = binary.LittleEndian.Uint64(b[i*8:])
	}
	return out, nil
}

func readU16(path string, n int) ([]uint16, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) != n*2 {
		return nil, fmt.Errorf("%s: %d bytes, expected %d", path, len(b), n*2)
	}
	out := make([]uint16, n)
	for i := range out {
		out[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	return out, nil
}

func readI32(path string, n int) ([]int32, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) != n*4 {
		return nil, fmt.Errorf("%s: %d bytes, expected %d", path, len(b), n*4)
	}
	out := make([]int32, n)
	for i := range out {
		out[i] = int32(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out, nil
}

// checkAligned verifies that row i of the dense index and row i of this index
// describe the SAME passage. Both are built from the same passages.jsonl.gz in
// file order, so a mismatch means one of them is stale — which would otherwise
// show up only as quietly worse ranking.
func (ix *colbertIndex) checkAligned(dense *quantIndex) error {
	if dense.Count != ix.Count {
		return fmt.Errorf("dense index has %d passages, colbert has %d — built from "+
			"different passage files", dense.Count, ix.Count)
	}
	for i := 0; i < ix.Count; i++ {
		if dense.ids[i] != ix.ids[i] {
			return fmt.Errorf("passage %d is page %d in the dense index but %d in colbert — "+
				"one of the two is stale", i, dense.ids[i], ix.ids[i])
		}
	}
	return nil
}

// queryWeights folds the per-dimension scale into the query once, so the inner
// loop is a small integer times a float with no per-dimension scale multiply.
// The divisor is the format's top code: int8 reconstructs at s*c/127, int4 at
// s*c/7, int2 at s*(2c-3)/3.
func (ix *colbertIndex) queryWeights(q [][]float32) [][]float32 {
	div := float32(127)
	switch ix.Bits {
	case 4:
		div = 7
	case 2:
		div = 3
	}
	w := make([][]float32, len(q))
	for i := range q {
		w[i] = make([]float32, ix.Dim)
		for d := 0; d < ix.Dim; d++ {
			w[i][d] = ix.scales[d] * q[i][d] / div
		}
	}
	return w
}

// buildLUTs precomputes, for every query token and every group of four
// dimensions, the partial score of all 256 possible packed bytes — the same
// trick the dense int2 scan uses, one table per query token.
//
// 32 query tokens x 24 groups x 256 floats is 786 KB, built once per query and
// then reused across every candidate: scoring a document token becomes 24
// lookups instead of 96 multiply-adds.
func (ix *colbertIndex) buildLUTs(w [][]float32) [][]float32 {
	groups := ix.stride
	luts := make([][]float32, len(w))
	for i := range w {
		lut := make([]float32, groups*256)
		for g := 0; g < groups; g++ {
			for b := 0; b < 256; b++ {
				var sum float32
				for j := 0; j < 4; j++ {
					d := g*4 + j
					if d >= ix.Dim {
						break
					}
					c := (b >> uint(2*j)) & 3
					sum += float32(2*c-3) * w[i][d]
				}
				lut[g*256+b] = sum
			}
		}
		luts[i] = lut
	}
	return luts
}

// maxSim scores one passage: for every query token, its single best-matching
// document token. Summed, never averaged — a passage that answers five query
// terms should beat one that answers two extremely well.
func (ix *colbertIndex) maxSim(w, luts [][]float32, offs []float32, row int, buf, dv []float32) float64 {
	n := int(ix.lens[row])
	if n == 0 {
		return 0
	}
	base := int(ix.offsets[row]) * ix.stride
	best := buf[:len(w)]
	for i := range best {
		best[i] = float32(math.Inf(-1))
	}
	for j := 0; j < n; j++ {
		tok := ix.tokens[base+j*ix.stride : base+(j+1)*ix.stride]

		if ix.Bits == 2 {
			// No decode at all: the packed byte indexes straight into the table.
			for i, lut := range luts {
				var s float32
				for g := 0; g < ix.stride; g++ {
					s += lut[g*256+int(tok[g])]
				}
				if s > best[i] {
					best[i] = s
				}
			}
			continue
		}

		// Decode the row once, then reuse it for all query tokens.
		switch ix.Bits {
		case 8:
			for d := 0; d < ix.Dim; d++ {
				dv[d] = float32(int8(tok[d]))
			}
		case 4:
			for d := 0; d < ix.Dim; d++ {
				b := tok[d/2]
				if d%2 == 0 {
					dv[d] = float32(int(b&0xF) - 7)
				} else {
					dv[d] = float32(int(b>>4) - 7)
				}
			}
		}
		for i := range w {
			wi := w[i]
			var s float32
			for d := 0; d < ix.Dim; d++ {
				s += dv[d] * wi[d]
			}
			if s > best[i] {
				best[i] = s
			}
		}
	}
	var total float64
	for i, b := range best {
		// The mean term is constant across passages, so it cannot change the
		// order; it is added back only to keep absolute scores comparable with
		// an uncentred (or float32) store.
		if offs != nil {
			total += float64(offs[i])
		}
		total += float64(b)
	}
	return total
}

// Rerank rescores candidate passage rows with MaxSim, in parallel.
// nbCounts derives a popularity proxy: how many passages a work contributed to
// the corpus, i.e. how long its article is. Free — the ids array is already
// loaded — and it is the same proxy notability.go uses.
func (ix *colbertIndex) nbCounts() map[int]float64 {
	if ix.nb != nil {
		return ix.nb
	}
	c := map[int]float64{}
	for _, id := range ix.ids {
		c[int(id)]++
	}
	ix.nb = map[int]float64{}
	for k, v := range c {
		ix.nb[k] = math.Log(1 + v)
	}
	return ix.nb
}

func (ix *colbertIndex) Rerank(q [][]float32, cand []int) []Hit {
	return ix.RerankOpts(q, cand, scoreOpts{})
}

func (ix *colbertIndex) RerankOpts(q [][]float32, cand []int, o scoreOpts) []Hit {
	if o.RealTokens > 0 && o.RealTokens < len(q) {
		q = q[:o.RealTokens]
	}
	hits := ix.rerank(q, cand)
	if o.LenNorm != 0 || o.Notability != 0 {
		nb := map[int]float64{}
		if o.Notability != 0 {
			nb = ix.nbCounts()
		}
		for i := range hits {
			if o.LenNorm != 0 {
				hits[i].Score -= o.LenNorm * math.Log(float64(ix.lens[hits[i].Passage])+1)
			}
			if o.Notability != 0 {
				hits[i].Score += o.Notability * nb[hits[i].PageID]
			}
		}
		sort.Slice(hits, func(a, b int) bool { return hits[a].Score > hits[b].Score })
	}
	return hits
}

func (ix *colbertIndex) rerank(q [][]float32, cand []int) []Hit {
	w := ix.queryWeights(q)
	var luts [][]float32
	if ix.Bits == 2 {
		luts = ix.buildLUTs(w)
	}
	var offs []float32
	if ix.mean != nil {
		offs = make([]float32, len(q))
		for i := range q {
			var s float32
			for d := 0; d < ix.Dim; d++ {
				s += ix.mean[d] * q[i][d]
			}
			offs[i] = s
		}
	}
	out := make([]Hit, len(cand))
	workers := runtime.NumCPU()
	if workers > len(cand) {
		workers = len(cand)
	}
	if workers < 1 {
		workers = 1
	}
	var wg sync.WaitGroup
	chunk := (len(cand) + workers - 1) / workers
	for wk := 0; wk < workers; wk++ {
		lo := wk * chunk
		hi := lo + chunk
		if hi > len(cand) {
			hi = len(cand)
		}
		if lo >= hi {
			continue
		}
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			buf := make([]float32, len(w))
			dv := make([]float32, ix.Dim)
			for k := lo; k < hi; k++ {
				r := cand[k]
				out[k] = Hit{
					PageID:  int(ix.ids[r]),
					Score:   ix.maxSim(w, luts, offs, r, buf, dv),
					Passage: r,
				}
			}
		}(lo, hi)
	}
	wg.Wait()
	sort.Slice(out, func(a, b int) bool { return out[a].Score > out[b].Score })
	return out
}

// colbertSearcher is the whole second path: dense retrieval for candidates,
// MaxSim for the ranking that is actually shown.
type colbertSearcher struct {
	dense  *quantIndex
	cb     *colbertIndex
	Coarse int // dense int2 candidates
	Deep   int // passages handed to MaxSim
	Opts   scoreOpts
}

func newColbertSearcher(dense *quantIndex, cb *colbertIndex) (*colbertSearcher, error) {
	if err := cb.checkAligned(dense); err != nil {
		return nil, err
	}
	return &colbertSearcher{dense: dense, cb: cb, Coarse: 4000, Deep: 500}, nil
}

// Search returns works ranked by their best passage's MaxSim score.
//
// Roll-up takes each work's best passage, matching the dense path, so the two
// are comparable: a film ranks on its strongest evidence, not on article length.
func (s *colbertSearcher) Search(qdense []float32, qtok [][]float32, n int) []Hit {
	return rollUpWorks(s.SearchPassages(qdense, qtok), n)
}

// SearchPassages stops before the roll-up, because the cross-encoder stage needs
// passages, not works — it scores (query, passage) pairs.
func (s *colbertSearcher) SearchPassages(qdense []float32, qtok [][]float32) []Hit {
	cand := s.dense.scanInt2(qdense, s.Coarse)
	dh := s.dense.rerankInt8(qdense, cand)
	if len(dh) > s.Deep {
		dh = dh[:s.Deep]
	}
	rows := make([]int, len(dh))
	for i, h := range dh {
		rows[i] = h.Passage
	}
	return s.cb.RerankOpts(qtok, rows, s.Opts)
}

// rollUpWorks keeps each work's best passage.
func rollUpWorks(scored []Hit, n int) []Hit {
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
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// embedQueryTokens asks the sidecar for the query's per-token matrix. Go cannot
// run the transformer; everything after this line is Go.
func embedQueryTokens(embedURL, q string, dim int) ([][]float32, error) {
	cl := &http.Client{Timeout: 120 * time.Second}
	resp, err := cl.Get(embedURL + "?q=" + url.QueryEscape(q))
	if err != nil {
		return nil, fmt.Errorf("colbert query encoder unreachable at %s: %w", embedURL, err)
	}
	defer resp.Body.Close()
	var out struct {
		Model  string    `json:"model"`
		Dim    int       `json:"dim"`
		Tokens int       `json:"tokens"`
		Vector []float32 `json:"vector"`
		Error  string    `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Error != "" {
		return nil, fmt.Errorf("colbert query encoder: %s", out.Error)
	}
	if out.Dim != dim {
		return nil, fmt.Errorf("query encoder returns dim %d, index is dim %d — the sidecar "+
			"is running a different checkpoint than the one that built the store", out.Dim, dim)
	}
	if out.Tokens == 0 || len(out.Vector) != out.Tokens*dim {
		return nil, fmt.Errorf("query encoder returned %d floats for %d tokens x %d dims",
			len(out.Vector), out.Tokens, dim)
	}
	rows := make([][]float32, out.Tokens)
	for i := range rows {
		rows[i] = out.Vector[i*dim : (i+1)*dim]
	}
	return rows, nil
}

// handleAPIColbert serves the late-interaction path to the browser.
func (s *server) handleAPIColbert(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]filmstock.UnifiedResult{})
		return
	}
	n := 25
	if v, err := strconv.Atoi(r.URL.Query().Get("n")); err == nil && v > 0 && v <= 100 {
		n = v
	}
	if s.late == nil {
		http.Error(w, "colbert search not configured (-colbert)", 503)
		return
	}
	// Route before spending a GPU round-trip and 10 MB of reads: if lexical is
	// confident the query IS a title, it answers better than ColBERT can (title
	// 1.000 vs 0.977, typo 0.863 vs 0.553) and answers in milliseconds.
	if s.routeAt > 0 {
		if res, err := filmstock.SearchMovies(r.Context(), s.db, q, "", n); err == nil &&
			len(res) > 0 && res[0].Score >= s.routeAt {
			hits := make([]Hit, 0, len(res))
			for _, m := range res {
				hits = append(hits, Hit{PageID: m.ID, Score: m.Score})
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(s.hitsToResults(hits))
			return
		}
	}
	qv, err := s.sem.embedQuery(q)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	if len(qv) != s.late.dense.Dim {
		http.Error(w, fmt.Sprintf("dense dimension mismatch: query %d, index %d",
			len(qv), s.late.dense.Dim), 500)
		return
	}
	qtok, err := embedQueryTokens(s.lateEmbed, q, s.late.cb.Dim)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	hits := s.late.Search(qv, qtok, n)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.hitsToResults(hits))
}

// readQueryTokens loads the [nq][qlen][dim] float32 matrix written by
// colbert.py queries.
func readQueryTokens(path string, nq, qlen, dim int) ([][][]float32, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	want := nq * qlen * dim * 4
	if len(b) != want {
		return nil, fmt.Errorf("%s: %d bytes, expected %d queries x %d tokens x %d dims = %d",
			path, len(b), nq, qlen, dim, want)
	}
	out := make([][][]float32, nq)
	p := 0
	for i := range out {
		out[i] = make([][]float32, qlen)
		for t := 0; t < qlen; t++ {
			row := make([]float32, dim)
			for d := 0; d < dim; d++ {
				row[d] = math.Float32frombits(binary.LittleEndian.Uint32(b[p:]))
				p += 4
			}
			out[i][t] = row
		}
	}
	return out, nil
}

// cmdEvalColbert scores the late-interaction path against the same query set as
// everything else, and reports the dense path beside it — the number that
// matters is the DELTA, and reranking can only reorder what dense retrieved.
func cmdEvalColbert(args []string) {
	fs := flag.NewFlagSet("eval-colbert", flag.ExitOnError)
	dbPath := fs.String("db", "out/search.db", "search database")
	setPath := fs.String("queries", "docs/eval/queries.json", "query set")
	manifest := fs.String("quant", "", "dense quant.<model>.json (required, supplies candidates)")
	idsPath := fs.String("ids", "out/index/passages.bin", "passage -> page_id map")
	qvecs := fs.String("qvecs", "", "dense query vectors from embed_queries.py (required)")
	cbMan := fs.String("colbert", "", "colbert.<model>.json (required)")
	cbQ := fs.String("cqvecs", "", "colbert query tokens from colbert.py queries (required)")
	dim := fs.Int("dim", 1024, "dense embedding dimension")
	n := fs.Int("n", 20, "retrieval depth")
	coarse := fs.Int("coarse", 4000, "int2 candidates")
	deep := fs.Int("deep", 500, "passages rescored with MaxSim")
	rerankURL := fs.String("rerank", "", "cross-encoder sidecar (embed/rerank.py serve); adds a third stage")
	rerankK := fs.Int("rerank-k", 50, "passages handed to the cross-encoder")
	realTok := fs.Int("qtokens", 0, "score over the first N query tokens only (0 = all; the rest are [MASK] expansion)")
	lenNorm := fs.Float64("lennorm", 0, "subtract L*log(passage tokens) — corrects the long-passage max bias")
	nbW := fs.Float64("nb", 0, "add W*log(1+passages of the work) — a notability prior")
	route := fs.Float64("route", 0, "if the best lexical hit scores >= this, answer from lexical instead of ColBERT")
	fs.Parse(args)

	for name, v := range map[string]string{"-quant": *manifest, "-qvecs": *qvecs,
		"-colbert": *cbMan, "-cqvecs": *cbQ} {
		if v == "" {
			fatal(fmt.Errorf("eval-colbert: %s is required", name))
		}
	}

	raw, err := os.ReadFile(*setPath)
	if err != nil {
		fatal(err)
	}
	var set evalSet
	if err := json.Unmarshal(raw, &set); err != nil {
		fatal(err)
	}

	dense, err := loadQuantIndex(*manifest, *idsPath)
	if err != nil {
		fatal(err)
	}
	cb, err := loadColbertIndex(*cbMan)
	if err != nil {
		fatal(err)
	}
	searcher, err := newColbertSearcher(dense, cb)
	if err != nil {
		fatal(err)
	}
	searcher.Coarse, searcher.Deep = *coarse, *deep
	searcher.Opts = scoreOpts{RealTokens: *realTok, LenNorm: *lenNorm, Notability: *nbW}
	if *realTok > 0 || *lenNorm != 0 || *nbW != 0 {
		fmt.Printf("scoring: qtokens=%d lennorm=%.3f nb=%.3f\n", *realTok, *lenNorm, *nbW)
	}

	qraw, err := os.ReadFile(*qvecs)
	if err != nil {
		fatal(err)
	}
	nq := len(qraw) / (*dim * 4)
	if nq != len(set.Queries) {
		fatal(fmt.Errorf("dense query vectors hold %d rows but the set has %d queries",
			nq, len(set.Queries)))
	}
	qv := make([][]float32, nq)
	for i := range qv {
		qv[i] = make([]float32, *dim)
		for d := 0; d < *dim; d++ {
			qv[i][d] = math.Float32frombits(binary.LittleEndian.Uint32(qraw[(i**dim+d)*4:]))
		}
	}

	var cbm colbertManifest
	mb, err := os.ReadFile(*cbMan)
	if err != nil {
		fatal(err)
	}
	if err := json.Unmarshal(mb, &cbm); err != nil {
		fatal(err)
	}
	qtok, err := readQueryTokens(*cbQ, nq, cbm.QueryMaxlen, cbm.Dim)
	if err != nil {
		fatal(err)
	}

	byQuery := map[string]int{}
	for i, q := range set.Queries {
		byQuery[q.Query] = i
	}
	idx := func(q string) (int, error) {
		i, ok := byQuery[q]
		if !ok {
			return 0, fmt.Errorf("no vector for %q", q)
		}
		return i, nil
	}

	denseOnly := func(q string, topn int) ([]int, error) {
		i, err := idx(q)
		if err != nil {
			return nil, err
		}
		hits := dense.Search(qv[i], *coarse, topn)
		ids := make([]int, len(hits))
		for j, h := range hits {
			ids[j] = h.PageID
		}
		return ids, nil
	}
	late := func(q string, topn int) ([]int, error) {
		i, err := idx(q)
		if err != nil {
			return nil, err
		}
		hits := searcher.Search(qv[i], qtok[i], topn)
		ids := make([]int, len(hits))
		for j, h := range hits {
			ids[j] = h.PageID
		}
		return ids, nil
	}

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		fatal(err)
	}
	defer db.Close()
	lexical := func(q string, topn int) ([]int, error) {
		res, err := filmstock.SearchMovies(context.Background(), db, q, "", topn)
		if err != nil {
			return nil, err
		}
		ids := make([]int, 0, len(res))
		for _, r := range res {
			ids = append(ids, r.ID)
		}
		return ids, nil
	}
	fused := func(q string, topn int) ([]int, error) {
		lex, err := lexical(q, topn)
		if err != nil {
			return nil, err
		}
		lt, err := late(q, topn)
		if err != nil {
			return nil, err
		}
		f := fuseRRF([]rankedList{{ids: lex}, {ids: lt}}, nil, rrfK)
		ids := make([]int, 0, len(f))
		for _, h := range f {
			ids = append(ids, h.PageID)
		}
		return ids, nil
	}

	fmt.Printf("\ncolbert: %s  dim=%d  %d passages  %.1f tokens/passage  %.2f GB\n",
		cbm.Model, cbm.Dim, cbm.Count, cbm.MeanTokens, float64(cbm.TokensBytes)/1e9)
	fmt.Printf("cascade: int2 %d -> int8 -> MaxSim %d\n", *coarse, *deep)

	// Routing, not fusion. Fusing the two lists measured WORSE than either input
	// three times over; but lexical genuinely owns two categories (title 1.000,
	// typo 0.884 against ColBERT's 0.977 and 0.553). The signal for "this is a
	// title query" is lexical's own confidence: a trigram-Dice score near 1 means
	// the query IS a title, modulo a typo. So ask lexical first and only keep its
	// answer when it is sure.
	if *route > 0 {
		routed := func(q string, topn int) ([]int, error) {
			res, err := filmstock.SearchMovies(context.Background(), db, q, "", topn)
			if err != nil {
				return nil, err
			}
			if len(res) > 0 && res[0].Score >= *route {
				ids := make([]int, 0, len(res))
				for _, r := range res {
					ids = append(ids, r.ID)
				}
				return ids, nil
			}
			return late(q, topn)
		}
		fmt.Printf("\nrouting threshold %.2f\n", *route)
		score(set, fmt.Sprintf("routed (lexical if >=%.2f else colbert)", *route), routed, *n, false)
		return
	}

	score(set, "dense (int2 -> int8)", denseOnly, *n, false)
	score(set, "colbert late interaction", late, *n, true)
	score(set, "colbert + lexical, RRF fused", fused, *n, false)

	if *rerankURL != "" {
		cross := &crossReranker{url: *rerankURL}
		crossed := func(q string, topn int) ([]int, error) {
			i, err := idx(q)
			if err != nil {
				return nil, err
			}
			// PASSAGES, not works. Rolling up first would hand the cross-encoder
			// only each work's best-by-MaxSim passage, so a film whose cast list
			// out-scored its own plot passage gets judged on the cast list —
			// measured at MRR 0.436 against 0.668 for not reranking at all.
			out, err := rerankHits(cross, q, searcher.SearchPassages(qv[i], qtok[i]), *rerankK, topn)
			if err != nil {
				return nil, err
			}
			ids := make([]int, len(out))
			for j, h := range out {
				ids[j] = h.PageID
			}
			return ids, nil
		}
		fmt.Printf("\ncross-encoder: %s, top %d passages\n", *rerankURL, *rerankK)
		score(set, "colbert -> cross-encoder", crossed, *n, true)
	}

	if *rerankURL != "" {
		cross := &crossReranker{url: *rerankURL}
		crossed := func(q string, topn int) ([]int, error) {
			i, err := idx(q)
			if err != nil {
				return nil, err
			}
			hits := rollUpWorks(searcher.SearchPassages(qv[i], qtok[i]), 10000)
			out, err := rerankHits(cross, q, hits, *rerankK, topn)
			if err != nil {
				return nil, err
			}
			ids := make([]int, len(out))
			for j, h := range out {
				ids[j] = h.PageID
			}
			return ids, nil
		}
		fmt.Printf("\ncross-encoder: %s over the top %d\n", *rerankURL, *rerankK)
		score(set, fmt.Sprintf("colbert -> cross-encoder (top %d)", *rerankK), crossed, *n, true)
	}
}
