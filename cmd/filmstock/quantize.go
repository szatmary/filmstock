package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Quantising float32 passage vectors down to the formats the device actually
// scans.
//
// The float32 blob is the artifact, not the deliverable: every level below is
// derived from it in one local pass, so producing several is nearly free and the
// GPU work is never repeated.
//
//	int8  — rerank format. mmap'd on the device, only the top ~1000 rows are
//	        touched per query, so its size barely matters.
//	int2  — the scan format, resident in RAM. 543,200 x 1024 dims = 139 MB, which
//	        a Raspberry Pi 4 can hold and stream in ~40 ms.
//
// PER-DIMENSION calibration is what makes 2 bits survivable. Embedding dimensions
// have very different dynamic ranges; a single global scale wastes most of the
// four available levels on dimensions that never approach it.
//
// The reconstruction levels are chosen so the device never has to multiply by a
// per-dimension scale during the scan. With
//
//	r[d][c] = scale[d] * (2c-3)/3      for c in 0..3   ->   -s, -s/3, +s/3, +s
//
// a dot product becomes
//
//	sum_d r[d][code_d] * q[d] = sum_d (2*code_d - 3) * (scale[d]*q[d]/3)
//
// so folding scale[d] into the query once per query leaves an inner loop of small
// odd integers (-3,-1,+1,+3) times a float — cheap to vectorise with NEON.

const int2Levels = 4

type quantManifest struct {
	Model         string    `json:"model"`
	Count         int       `json:"count"`
	Dim           int       `json:"dim"`
	Scales        []float32 `json:"scales_int8"`
	Scales2       []float32 `json:"scales_int2"`
	Int8Path      string    `json:"int8_path"`
	Int2Path      string    `json:"int2_path"`
	Int8Bytes     int64     `json:"int8_bytes"`
	Int2Bytes     int64     `json:"int2_bytes"`
	CosineInt8    float64   `json:"mean_cosine_int8"`
	CosineInt2    float64   `json:"mean_cosine_int2"`
	CalibPercent  float64   `json:"calibration_percentile"`
	ElapsedSecond float64   `json:"elapsed_s"`
}

func cmdQuantize(args []string) {
	fs := flag.NewFlagSet("quantize", flag.ExitOnError)
	in := fs.String("vectors", "", "float32 vector blob from embed.py (required)")
	dim := fs.Int("dim", 1024, "embedding dimension")
	outDir := fs.String("out", "", "output dir (default: alongside -vectors)")
	pct := fs.Float64("percentile", 99.9, "per-dimension calibration percentile")
	sample := fs.Int("sample", 100000, "vectors sampled for calibration and error measurement")
	fs.Parse(args)

	if *in == "" {
		fatal(fmt.Errorf("quantize: -vectors is required"))
	}
	if *outDir == "" {
		*outDir = filepath.Dir(*in)
	}
	start := time.Now()

	st, err := os.Stat(*in)
	if err != nil {
		fatal(err)
	}
	rowBytes := int64(*dim) * 4
	if st.Size()%rowBytes != 0 {
		fatal(fmt.Errorf("blob size %d is not a multiple of dim*4 (%d) — wrong -dim?",
			st.Size(), rowBytes))
	}
	count := int(st.Size() / rowBytes)
	fmt.Fprintf(os.Stderr, "quantizing %d vectors x %d dims from %s\n", count, *dim, *in)

	f, err := os.Open(*in)
	if err != nil {
		fatal(err)
	}
	defer f.Close()

	// --- pass 1: per-dimension calibration from a sample ----------------------
	step := count / max(*sample, 1)
	if step < 1 {
		step = 1
	}
	cols := make([][]float32, *dim)
	for d := range cols {
		cols[d] = make([]float32, 0, count/step+1)
	}
	row := make([]float32, *dim)
	nSample := 0
	for i := 0; i < count; i += step {
		if err := readRow(f, int64(i)*rowBytes, row); err != nil {
			fatal(err)
		}
		for d, v := range row {
			cols[d] = append(cols[d], float32(math.Abs(float64(v))))
		}
		nSample++
	}
	// TWO calibrations, because the formats want opposite things.
	//
	// int8 has 255 levels, so it wants a WIDE scale (a high percentile of |v|):
	// clipping is the only real risk and resolution is abundant.
	//
	// int2 has four. A wide scale puts its outer reconstruction points far out in
	// the tail where almost no mass lives, wasting half the codebook — which is
	// exactly why the first version scored 0.88. Embedding dimensions are roughly
	// Gaussian, and the optimal uniform 4-level quantiser for a Gaussian places
	// levels at ~±0.5σ and ~±1.5σ. With r = s*(2c-3)/3 that means s = 1.494σ.
	scales := make([]float32, *dim)  // int8
	scales2 := make([]float32, *dim) // int2
	for d := range cols {
		sort.Slice(cols[d], func(a, b int) bool { return cols[d][a] < cols[d][b] })
		idx := int(float64(len(cols[d])-1) * *pct / 100)
		s := cols[d][idx]
		if s <= 0 {
			s = 1e-6 // a dead dimension must not divide by zero
		}
		scales[d] = s

		// |v| was collected, so E[|v|] = sigma*sqrt(2/pi) for a zero-mean Gaussian.
		var sum float64
		for _, a := range cols[d] {
			sum += float64(a)
		}
		sigma := (sum / float64(len(cols[d]))) / 0.7978845608
		s2 := float32(1.494 * sigma)
		if s2 <= 0 {
			s2 = 1e-6
		}
		scales2[d] = s2
	}
	fmt.Fprintf(os.Stderr, "  calibrated on %d vectors: int8 p%.1f %.4f..%.4f, int2 1.494sigma %.4f..%.4f\n",
		nSample, *pct, minf(scales), maxf(scales), minf(scales2), maxf(scales2))

	// --- pass 2: encode -------------------------------------------------------
	tag := trimExt(filepath.Base(*in))
	i8Path := filepath.Join(*outDir, "vectors.int8."+tag+".bin")
	i2Path := filepath.Join(*outDir, "vectors.int2."+tag+".bin")
	i8f, err := os.Create(i8Path)
	if err != nil {
		fatal(err)
	}
	defer i8f.Close()
	i2f, err := os.Create(i2Path)
	if err != nil {
		fatal(err)
	}
	defer i2f.Close()

	i8buf := make([]byte, *dim)
	i2buf := make([]byte, (*dim+3)/4)
	var sumCos8, sumCos2 float64
	var nErr int

	for i := 0; i < count; i++ {
		if err := readRow(f, int64(i)*rowBytes, row); err != nil {
			fatal(err)
		}
		for d, v := range row {
			s := scales[d]
			// int8: symmetric, clipped at the calibration percentile.
			q := int(math.Round(float64(v/s) * 127))
			if q > 127 {
				q = 127
			}
			if q < -127 {
				q = -127
			}
			i8buf[d] = byte(int8(q))

			// int2: four levels at -s2, -s2/3, +s2/3, +s2.
			s2 := scales2[d]
			c := int(math.Round((float64(v/s2)*3 + 3) / 2))
			if c < 0 {
				c = 0
			}
			if c > 3 {
				c = 3
			}
			shift := uint((d % 4) * 2)
			if shift == 0 {
				i2buf[d/4] = 0
			}
			i2buf[d/4] |= byte(c) << shift
		}
		if _, err := i8f.Write(i8buf); err != nil {
			fatal(err)
		}
		if _, err := i2f.Write(i2buf); err != nil {
			fatal(err)
		}

		// Measure what quantisation actually cost, on the same stride used for
		// calibration. Reporting this is the difference between "2-bit works" as
		// an assumption and as a fact.
		if i%step == 0 {
			sumCos8 += cosineToReconstructed(row, i8buf, scales, 8)
			sumCos2 += cosineToReconstructed(row, i2buf, scales2, 2)
			nErr++
		}
		if i%100000 == 0 && i > 0 {
			fmt.Fprintf(os.Stderr, "\r  %d/%d", i, count)
		}
	}

	i8st, _ := i8f.Stat()
	i2st, _ := i2f.Stat()
	man := quantManifest{
		Model: tag, Count: count, Dim: *dim, Scales: scales, Scales2: scales2,
		Int8Path: i8Path, Int2Path: i2Path,
		Int8Bytes: i8st.Size(), Int2Bytes: i2st.Size(),
		CosineInt8:    sumCos8 / float64(max(nErr, 1)),
		CosineInt2:    sumCos2 / float64(max(nErr, 1)),
		CalibPercent:  *pct,
		ElapsedSecond: time.Since(start).Seconds(),
	}
	mp := filepath.Join(*outDir, "quant."+tag+".json")
	b, _ := json.MarshalIndent(man, "", "  ")
	os.WriteFile(mp, b, 0o644)

	fmt.Fprintf(os.Stderr, "\r  done in %.1fs\n", man.ElapsedSecond)
	fmt.Fprintf(os.Stderr, "  int8  %7.1f MB   mean cosine to float32  %.4f\n",
		float64(man.Int8Bytes)/1e6, man.CosineInt8)
	fmt.Fprintf(os.Stderr, "  int2  %7.1f MB   mean cosine to float32  %.4f\n",
		float64(man.Int2Bytes)/1e6, man.CosineInt2)
	fmt.Fprintf(os.Stderr, "  manifest %s\n", mp)
}

// cosineToReconstructed decodes a quantised row back to floats and reports its
// cosine similarity with the original. bits selects the format.
func cosineToReconstructed(orig []float32, code []byte, scales []float32, bits int) float64 {
	var dot, na, nb float64
	for d := range orig {
		var r float64
		if bits == 8 {
			r = float64(int8(code[d])) / 127 * float64(scales[d])
		} else {
			c := (code[d/4] >> uint((d%4)*2)) & 3
			r = float64(scales[d]) * float64(2*int(c)-3) / 3
		}
		a := float64(orig[d])
		dot += a * r
		na += a * a
		nb += r * r
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func readRow(f *os.File, off int64, row []float32) error {
	buf := make([]byte, len(row)*4)
	if _, err := f.ReadAt(buf, off); err != nil {
		return err
	}
	for i := range row {
		row[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
	}
	return nil
}

// trimExt turns "vectors.f32.<model>.bin" into "<model>", so the derived files
// are vectors.int2.<model>.bin rather than vectors.int2.f32.<model>.bin.
func trimExt(s string) string {
	s = strings.TrimSuffix(s, ".bin")
	s = strings.TrimPrefix(s, "vectors.")
	s = strings.TrimPrefix(s, "f32.")
	return s
}

func minf(a []float32) float32 {
	m := a[0]
	for _, v := range a {
		if v < m {
			m = v
		}
	}
	return m
}

func maxf(a []float32) float32 {
	m := a[0]
	for _, v := range a {
		if v > m {
			m = v
		}
	}
	return m
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
