package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Requantising the ColBERT token store below int8.
//
// This needs no GPU and no re-encode. int8 at p99.9 reconstructs the float32
// vectors to within 1% (colbert_test.go measures it against the encoder), so
// int8 -> int4/int2 is within noise of float32 -> int4/int2 and costs one
// streaming pass instead of a 15-minute GPU job.
//
// Why it matters here and not on the array: on /tank the cost of a query is
// SEEKS (500 scattered reads at ~100 IOPS), and a smaller row does not make a
// seek cheaper. On the storage this is actually meant to ship on — a card with
// fast seek and modest throughput — the cost is BYTES, and every level down is a
// near-linear win:
//
//	int8   13.25 GB   21.8 KB/passage    (the encoder's output)
//	int4    6.63 GB   10.9 KB/passage
//	int2    3.31 GB    5.5 KB/passage
//
// Two calibrations, for the same reason quantize.go has two: int4 has 15 levels
// and wants a WIDE scale because clipping is the only real risk, while int2 has
// four and wants 1.494*sigma, which places them at the optimum for a Gaussian.
// A wide scale at 2 bits spends half the codebook out in the tail.
//
// The risk that is specific to late interaction: MaxSim takes a MAX over token
// scores, so quantisation error does not average out the way it does inside a
// dot product — the max is biased upward by whichever token got the luckiest
// rounding, 32 query tokens deep. That cannot be reasoned about, only measured,
// which is what eval-colbert against the 0.697 baseline is for.

func cmdColbertQuantize(args []string) {
	fs := flag.NewFlagSet("colbert-quantize", flag.ExitOnError)
	in := fs.String("colbert", "", "colbert.<model>.json holding the int8 store (required)")
	bits := fs.Int("bits", 2, "target width: 4 or 2")
	outDir := fs.String("out", "", "output dir (default: alongside the source)")
	sample := fs.Int("sample", 500000, "token vectors sampled for calibration")
	pct := fs.Float64("percentile", 99.9, "int4 calibration percentile")
	center := fs.Bool("center", false, "quantise the residual from the mean token vector")
	fs.Parse(args)

	if *in == "" {
		fatal(fmt.Errorf("colbert-quantize: -colbert is required"))
	}
	if *bits != 4 && *bits != 2 {
		fatal(fmt.Errorf("colbert-quantize: -bits must be 4 or 2 (the source is int8)"))
	}
	raw, err := os.ReadFile(*in)
	if err != nil {
		fatal(err)
	}
	var man colbertManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		fatal(err)
	}
	if man.Bits != 8 {
		fatal(fmt.Errorf("%s holds a %d-bit store; requantising is only defined from int8",
			*in, man.Bits))
	}
	if *outDir == "" {
		*outDir = filepath.Dir(man.TokensPath)
	}
	start := time.Now()

	dim := man.Dim
	total := man.TotalTokens
	fmt.Fprintf(os.Stderr, "requantising %d token vectors x %d dims: int8 -> int%d\n",
		total, dim, *bits)

	// --- pass 1: calibrate ----------------------------------------------------
	//
	// Sampled from a sequential stream rather than by seeking to random rows:
	// the store is 13 GB on an array that does ~100 IOPS, where half a million
	// seeks would cost more than reading the whole file.
	step := int(total / int64(max(*sample, 1)))
	if step < 1 {
		step = 1
	}
	cols := make([][]float32, dim)
	for d := range cols {
		cols[d] = make([]float32, 0, *sample+1)
	}
	f, err := os.Open(man.TokensPath)
	if err != nil {
		fatal(err)
	}
	r := bufio.NewReaderSize(f, 1<<22)
	row := make([]byte, dim)
	nSample := 0
	vals := make([][]float32, dim) // signed, kept only when centring
	sum := make([]float64, dim)
	for i := int64(0); i < total; i++ {
		if _, err := readFull(r, row); err != nil {
			fatal(err)
		}
		if i%int64(step) != 0 {
			continue
		}
		for d := 0; d < dim; d++ {
			v := man.Scales[d] * float32(int8(row[d])) / 127
			sum[d] += float64(v)
			if *center {
				vals[d] = append(vals[d], v)
			} else {
				cols[d] = append(cols[d], float32(math.Abs(float64(v))))
			}
		}
		nSample++
	}
	f.Close()

	// Centring: every ColBERT token vector shares a large common direction, so
	// the codebook spends its resolution describing what all tokens have in
	// common instead of what distinguishes them. Quantising the residual puts
	// the bits where the ranking signal is.
	//
	// It costs nothing at scoring time: max_j (mean.q_i + resid_j.q_i) =
	// mean.q_i + max_j (resid_j.q_i), and that constant is the same for every
	// passage, so it cannot change the order. It is added back only so absolute
	// scores stay comparable with float32.
	mean := make([]float32, dim)
	if *center {
		for d := 0; d < dim; d++ {
			mean[d] = float32(sum[d] / float64(max(nSample, 1)))
			cols[d] = cols[d][:0]
			for _, v := range vals[d] {
				cols[d] = append(cols[d], float32(math.Abs(float64(v-mean[d]))))
			}
			vals[d] = nil
		}
		var mn float64
		for _, m := range mean {
			mn += float64(m) * float64(m)
		}
		fmt.Fprintf(os.Stderr, "  centred: mean token vector has norm %.4f\n", math.Sqrt(mn))
	}

	scales := make([]float32, dim)
	for d := range cols {
		if len(cols[d]) == 0 {
			fatal(fmt.Errorf("dimension %d got no samples", d))
		}
		var s float32
		if *bits == 4 {
			sort.Slice(cols[d], func(a, b int) bool { return cols[d][a] < cols[d][b] })
			s = cols[d][int(float64(len(cols[d])-1)**pct/100)]
		} else {
			// E[|v|] = sigma*sqrt(2/pi) for zero-mean Gaussian; s = 1.494*sigma
			// puts the four reconstruction points at +-0.5 and +-1.5 sigma.
			var sum float64
			for _, a := range cols[d] {
				sum += float64(a)
			}
			sigma := (sum / float64(len(cols[d]))) / 0.7978845608
			s = float32(1.494 * sigma)
		}
		if s <= 0 {
			s = 1e-6 // a dead dimension must not divide by zero
		}
		scales[d] = s
	}
	fmt.Fprintf(os.Stderr, "  calibrated on %d token vectors: scale %.4f..%.4f\n",
		nSample, minf(scales), maxf(scales))

	// --- pass 2: encode -------------------------------------------------------
	tag := man.Model
	for i := 0; i < len(tag); i++ {
		if tag[i] == '/' {
			tag = tag[:i] + "_" + tag[i+1:]
		}
	}
	suffix := ""
	if *center {
		suffix = "c"
	}
	outPath := filepath.Join(*outDir, fmt.Sprintf("colbert.tokens.int%d%s.%s.bin", *bits, suffix, tag))
	f, err = os.Open(man.TokensPath)
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	of, err := os.Create(outPath)
	if err != nil {
		fatal(err)
	}
	defer of.Close()
	r = bufio.NewReaderSize(f, 1<<22)
	w := bufio.NewWriterSize(of, 1<<22)

	stride := colbertStride(dim, *bits)
	packed := make([]byte, stride)
	rec := make([]float32, dim)
	src := make([]float32, dim)
	var sumCos float64
	var nCos int

	for i := int64(0); i < total; i++ {
		if _, err := readFull(r, row); err != nil {
			fatal(err)
		}
		for d := 0; d < dim; d++ {
			src[d] = man.Scales[d]*float32(int8(row[d]))/127 - mean[d]
		}
		for j := range packed {
			packed[j] = 0
		}
		for d := 0; d < dim; d++ {
			x := src[d] / scales[d]
			if *bits == 4 {
				c := int(math.Round(float64(x * 7)))
				c = clampInt(c, -7, 7)
				rec[d] = scales[d] * float32(c) / 7
				n := byte(c + 7) // 0..14, so the nibble is unsigned
				if d%2 == 0 {
					packed[d/2] |= n
				} else {
					packed[d/2] |= n << 4
				}
			} else {
				// r = s*(2c-3)/3  ->  c = (3x+3)/2
				c := int(math.Round((3*float64(x) + 3) / 2))
				c = clampInt(c, 0, 3)
				rec[d] = scales[d] * float32(2*c-3) / 3
				packed[d/4] |= byte(c) << uint(2*(d%4))
			}
		}
		if _, err := w.Write(packed); err != nil {
			fatal(err)
		}
		if i%1000 == 0 {
			sumCos += cosine(src, rec)
			nCos++
		}
		if i%10_000_000 == 0 && i > 0 {
			fmt.Fprintf(os.Stderr, "\r  %d/%d tokens", i, total)
		}
	}
	if err := w.Flush(); err != nil {
		fatal(err)
	}
	st, err := of.Stat()
	if err != nil {
		fatal(err)
	}

	out := man // offsets, lens and page ids are unchanged: same rows, same order
	out.Bits = *bits
	out.Scales = scales
	out.TokensPath = outPath
	if *center {
		out.Mean = mean
	}
	out.TokensBytes = st.Size()
	out.ElapsedSecond = time.Since(start).Seconds()
	out.CosineToInt8 = sumCos / float64(max(nCos, 1))

	mp := filepath.Join(*outDir, fmt.Sprintf("colbert.int%d%s.%s.json", *bits, suffix, tag))
	b, _ := json.MarshalIndent(out, "", "  ")
	if err := os.WriteFile(mp, b, 0o644); err != nil {
		fatal(err)
	}

	fmt.Fprintf(os.Stderr, "\r  int%d   %7.2f GB   mean cosine to int8  %.4f   (%.0fx smaller)\n",
		*bits, float64(st.Size())/1e9, out.CosineToInt8, float64(man.TokensBytes)/float64(st.Size()))
	fmt.Fprintf(os.Stderr, "  %.1f KB per passage at %.1f tokens\n",
		float64(stride)*man.MeanTokens/1000, man.MeanTokens)
	fmt.Fprintf(os.Stderr, "  manifest %s  (%.1fs)\n", mp, out.ElapsedSecond)
}

// colbertStride is the packed row width for one token vector.
func colbertStride(dim, bits int) int {
	switch bits {
	case 8:
		return dim
	case 4:
		return (dim + 1) / 2
	case 2:
		return (dim + 3) / 4
	}
	return 0
}

func readFull(r interface{ Read([]byte) (int, error) }, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := r.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
