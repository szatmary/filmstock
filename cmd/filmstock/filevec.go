package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Film-level vectors for the similarity browser.
//
// The passage index has 1-30 vectors per film, which is right for search (a
// query should be able to match the Box office section specifically) but wrong
// for browsing, where a film needs ONE position in the space.
//
// Two ways to collapse them, and the choice dominates the quality of the whole
// browser:
//
//	lead     — passage 0, the article's opening paragraph: title, year, director,
//	           cast, premise. The closest thing to "what this film is".
//	centroid — mean of all passages. Smoother, but for a famous film most passages
//	           are production, marketing and home-media detail, so The Martian's
//	           centroid drifts toward Wadi Rum and 4DX release formats rather than
//	           a man stranded on Mars.
//
// Both are built so they can be compared rather than assumed.
//
// Note this index needs NO query encoder: browsing moves between vectors that
// already exist, so it runs on a Pi or in a browser with no model at all.

func cmdFilmVectors(args []string) {
	fs := flag.NewFlagSet("filmvecs", flag.ExitOnError)
	in := fs.String("vectors", "", "passage float32 blob (required)")
	idsPath := fs.String("ids", "", "passages.bin (default alongside -vectors)")
	dim := fs.Int("dim", 1024, "embedding dimension")
	mode := fs.String("mode", "lead", "lead | centroid")
	out := fs.String("out", "", "output prefix (default alongside -vectors)")
	fs.Parse(args)

	if *in == "" {
		fatal(fmt.Errorf("filmvecs: -vectors is required"))
	}
	dir := filepath.Dir(*in)
	if *idsPath == "" {
		*idsPath = filepath.Join(dir, "passages.bin")
	}
	if *out == "" {
		*out = filepath.Join(dir, "films."+*mode)
	}
	start := time.Now()

	idsRaw, err := os.ReadFile(*idsPath)
	if err != nil {
		fatal(err)
	}
	ids := make([]int32, len(idsRaw)/4)
	for i := range ids {
		ids[i] = int32(binary.LittleEndian.Uint32(idsRaw[i*4:]))
	}

	f, err := os.Open(*in)
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	rowBytes := int64(*dim) * 4
	st, _ := f.Stat()
	count := int(st.Size() / rowBytes)
	if count > len(ids) {
		fatal(fmt.Errorf("blob has %d rows, passages.bin has %d ids", count, len(ids)))
	}

	acc := map[int32][]float64{}
	n := map[int32]int{}
	seen := map[int32]bool{}
	row := make([]float32, *dim)
	var order []int32

	for i := 0; i < count; i++ {
		if err := readRow(f, int64(i)*rowBytes, row); err != nil {
			fatal(err)
		}
		id := ids[i]
		if !seen[id] {
			seen[id] = true
			order = append(order, id)
		}
		if *mode == "lead" {
			if n[id] == 0 { // passages are written in order, so the first is the lead
				v := make([]float64, *dim)
				for d := range row {
					v[d] = float64(row[d])
				}
				acc[id] = v
				n[id] = 1
			}
			continue
		}
		if acc[id] == nil {
			acc[id] = make([]float64, *dim)
		}
		for d := range row {
			acc[id][d] += float64(row[d])
		}
		n[id]++
	}

	sort.Slice(order, func(a, b int) bool { return order[a] < order[b] })
	vecPath := *out + ".f32.bin"
	idPath := *out + ".ids.bin"
	vf, err := os.Create(vecPath)
	if err != nil {
		fatal(err)
	}
	defer vf.Close()
	idf, err := os.Create(idPath)
	if err != nil {
		fatal(err)
	}
	defer idf.Close()

	buf := make([]byte, *dim*4)
	idbuf := make([]byte, 4)
	for _, id := range order {
		v := acc[id]
		// Re-normalise: averaging breaks unit length, and every downstream score
		// is a dot product that assumes it.
		var norm float64
		for _, x := range v {
			norm += x * x
		}
		norm = math.Sqrt(norm)
		if norm == 0 {
			norm = 1
		}
		for d, x := range v {
			binary.LittleEndian.PutUint32(buf[d*4:], math.Float32bits(float32(x/norm)))
		}
		vf.Write(buf)
		binary.LittleEndian.PutUint32(idbuf, uint32(id))
		idf.Write(idbuf)
	}
	fmt.Fprintf(os.Stderr, "%s: %d films x %d dims -> %s (%.1fs)\n",
		*mode, len(order), *dim, vecPath, time.Since(start).Seconds())
}
