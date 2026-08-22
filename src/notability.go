package main

import (
	"compress/gzip"
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Notability as an APPENDED VECTOR DIMENSION rather than a ranking step.
//
// Scoring is a dot product, so appending one dimension to every document vector
// (value n) and setting the query's matching component to lambda gives
//
//	dot([d,n],[q,λ]) = dot(d,q) + λ·n
//
// which is exactly "semantic score + λ × notability" — but living inside the
// index. No ranking code, no post-processing, and λ is chosen AT QUERY TIME, so
// the weight can be swept or switched off without re-embedding 606k passages.
//
// This is the right home for it because notability is query-INDEPENDENT: it is a
// per-document constant, not a similarity. Fine-tuning it into 1024 dimensions
// would entangle a scalar with a similarity function, waste capacity, and cost a
// full re-embed to change.
//
// The signal itself is article length, which is a proxy. Incoming wikilink count
// is the principled measure, but pagelinks.sql.gz is not among the downloaded
// dumps; when it is, only buildNotability changes — the geometry above does not.

// notabilityFor returns a 0..1 score per page_id from the corpus text length.
//
// log10 then rank-normalised: raw length is heavily skewed (p10=836, median=2442,
// p90=8599, max=38845 chars), so a linear scale would let a handful of very long
// articles dominate while leaving the middle of the distribution flat.
func buildNotability(recordsDir string) (map[int]float32, error) {
	type ent struct {
		id  int
		val float64
	}
	var all []ent
	err := walkRecords(recordsDir, kindText, func(p string) error {
		id, ok := pageIDFromPath(p)
		if !ok {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		zr, err := gzip.NewReader(f)
		if err != nil {
			return nil
		}
		defer zr.Close()
		var n int64
		buf := make([]byte, 32<<10)
		for {
			k, err := zr.Read(buf)
			n += int64(k)
			if err != nil {
				break
			}
		}
		if n > 0 {
			all = append(all, ent{id, math.Log10(float64(n))})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("no text records under %s", recordsDir)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].val < all[j].val })
	out := make(map[int]float32, len(all))
	for i, e := range all {
		out[e.id] = float32(i) / float32(len(all)-1) // rank-normalised to 0..1
	}
	return out, nil
}

// cmdNotability appends the notability dimension to an existing float32 vector
// blob, producing a blob of dim+1 that quantize/search treat as ordinary.
func cmdNotability(args []string) {
	fs := flag.NewFlagSet("notability", flag.ExitOnError)
	records := fs.String("records", "/tank/mediadb/out", "record hierarchy")
	in := fs.String("vectors", "", "float32 vector blob (required)")
	idsPath := fs.String("ids", "", "passages.bin (default alongside -vectors)")
	dim := fs.Int("dim", 1024, "embedding dimension of the input blob")
	out := fs.String("out", "", "output blob (default <vectors dir>/vectors.f32.<tag>+nb.bin)")
	fs.Parse(args)

	if *in == "" {
		fatal(fmt.Errorf("notability: -vectors is required"))
	}
	if *idsPath == "" {
		*idsPath = filepath.Join(filepath.Dir(*in), "passages.bin")
	}
	if *out == "" {
		*out = filepath.Join(filepath.Dir(*in), "vectors.f32."+trimExt(filepath.Base(*in))+"+nb.bin")
	}
	start := time.Now()

	fmt.Fprintln(os.Stderr, "building notability from article length...")
	nb, err := buildNotability(*records)
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "  %d works scored\n", len(nb))

	idsRaw, err := os.ReadFile(*idsPath)
	if err != nil {
		fatal(err)
	}
	ids := make([]int32, len(idsRaw)/4)
	for i := range ids {
		ids[i] = int32(binary.LittleEndian.Uint32(idsRaw[i*4:]))
	}

	src, err := os.Open(*in)
	if err != nil {
		fatal(err)
	}
	defer src.Close()
	dst, err := os.Create(*out)
	if err != nil {
		fatal(err)
	}
	defer dst.Close()

	rowBytes := *dim * 4
	buf := make([]byte, rowBytes+4)
	st, _ := src.Stat()
	count := int(st.Size()) / rowBytes
	if count > len(ids) {
		fatal(fmt.Errorf("blob has %d rows but passages.bin has %d ids", count, len(ids)))
	}
	for i := 0; i < count; i++ {
		if _, err := src.Read(buf[:rowBytes]); err != nil {
			fatal(err)
		}
		binary.LittleEndian.PutUint32(buf[rowBytes:], math.Float32bits(nb[int(ids[i])]))
		if _, err := dst.Write(buf); err != nil {
			fatal(err)
		}
	}
	fmt.Fprintf(os.Stderr, "wrote %d rows x %d dims to %s in %.1fs\n",
		count, *dim+1, *out, time.Since(start).Seconds())
	fmt.Fprintf(os.Stderr, "  quantize with -dim %d; set the query's last component to lambda\n", *dim+1)
}
