package build

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
)

// Quantising the float32 embeddings into the file that actually ships.
//
// Per-DIMENSION scaling, not a global one. With a single global range the
// low-variance dimensions are crushed to nothing and roughly half the near
// neighbours change; per-dimension keeps 99.4% of the top 20 at a quarter of the
// size. Measured on 20,000 films: float32 666 MB / 1.000, int8 166 MB / 0.994,
// int4 83 MB / 0.905, int2 42 MB / 0.597.
//
// The range itself comes from -scale and is not derived here unless -new-epoch
// says to. See Scale for why: recomputing it per build is what made this file
// impossible to ship as a diff.
func CmdVectors(args []string) {
	fs := flag.NewFlagSet("vectors", flag.ExitOnError)
	in := fs.String("in", "", "float32 vectors (count × dims, row-major)")
	idsPath := fs.String("ids", "", "int32 page_id per row")
	out := fs.String("out", "vectors.bin", "quantised output")
	dims := fs.Int("dims", 1024, "values per row")
	scalePath := fs.String("scale", "", "frozen per-dimension quantisation range")
	newEpoch := fs.Bool("new-epoch", false, "derive the range from this corpus and write it to -scale, starting a new epoch")
	fs.Parse(args)
	if *in == "" || *idsPath == "" || *scalePath == "" {
		fatal(fmt.Errorf("vectors needs -in, -ids and -scale"))
	}

	idb, err := os.ReadFile(*idsPath)
	if err != nil {
		fatal(err)
	}
	count := len(idb) / 4
	raw, err := os.ReadFile(*in)
	if err != nil {
		fatal(err)
	}
	if len(raw) != count**dims*4 {
		fatal(fmt.Errorf("%s is %d bytes; %d rows × %d dims × 4 = %d",
			*in, len(raw), count, *dims, count**dims*4))
	}
	at := func(r, d int) float32 {
		return math.Float32frombits(binary.LittleEndian.Uint32(raw[(r**dims+d)*4:]))
	}

	scale, err := LoadScale(*scalePath)
	switch {
	case err == nil && *newEpoch:
		fatal(fmt.Errorf("-new-epoch but %s already exists; move it aside to start an epoch deliberately", *scalePath))
	case errors.Is(err, os.ErrNotExist) && !*newEpoch:
		fatal(fmt.Errorf("%s: %w; pass -new-epoch to derive one, knowing it invalidates every vector file built against the old range", *scalePath, ErrNoScale))
	case errors.Is(err, os.ErrNotExist):
		scale = DeriveScale(at, count, *dims)
		if err := scale.Write(*scalePath); err != nil {
			fatal(err)
		}
		fmt.Fprintf(os.Stderr, "derived new epoch scale %s -> %s\n", scale.ID(), *scalePath)
	case err != nil:
		fatal(err)
	}
	if scale.Dims != *dims {
		fatal(fmt.Errorf("%s is %d dims, -dims is %d", *scalePath, scale.Dims, *dims))
	}
	lo, hi := scale.Lo, scale.Hi

	f, err := os.Create(*out)
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	w := make([]byte, 0, 1<<20)
	w = append(w, "fsvec1\n"...)
	w = binary.LittleEndian.AppendUint32(w, uint32(count))
	w = binary.LittleEndian.AppendUint32(w, uint32(*dims))
	for d := range *dims {
		w = binary.LittleEndian.AppendUint32(w, math.Float32bits(lo[d]))
		w = binary.LittleEndian.AppendUint32(w, math.Float32bits(hi[d]))
	}
	w = append(w, idb...)
	if _, err := f.Write(w); err != nil {
		fatal(err)
	}

	// Values outside a frozen range clamp. That is the price of the range not
	// moving, so it is counted and reported rather than left to be discovered.
	row := make([]byte, *dims)
	var clamped int64
	for r := range count {
		for d := range *dims {
			span := hi[d] - lo[d]
			var q float32
			if span > 0 {
				q = (at(r, d) - lo[d]) / span * 255
			}
			if q < 0 || q > 255 {
				clamped++
			}
			row[d] = uint8(math.Round(float64(min(max(q, 0), 255))))
		}
		if _, err := f.Write(row); err != nil {
			fatal(err)
		}
	}
	fi, _ := f.Stat()
	fmt.Fprintf(os.Stderr, "%s: %d rows × %d dims, %.0f MB (from %.0f MB float32)\n",
		*out, count, *dims, float64(fi.Size())/(1<<20), float64(len(raw))/(1<<20))
	fmt.Fprintf(os.Stderr, "scale %s: %d of %d values clamped (%.4f%%)\n",
		scale.ID(), clamped, int64(count)*int64(*dims),
		float64(clamped)*100/(float64(count)*float64(*dims)))
}
