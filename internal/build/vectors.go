package build

import (
	"encoding/binary"
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
func CmdVectors(args []string) {
	fs := flag.NewFlagSet("vectors", flag.ExitOnError)
	in := fs.String("in", "", "float32 vectors (count × dims, row-major)")
	idsPath := fs.String("ids", "", "int32 page_id per row")
	out := fs.String("out", "vectors.bin", "quantised output")
	dims := fs.Int("dims", 1024, "values per row")
	fs.Parse(args)
	if *in == "" || *idsPath == "" {
		fatal(fmt.Errorf("vectors needs -in and -ids"))
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

	lo := make([]float32, *dims)
	hi := make([]float32, *dims)
	for d := range *dims {
		lo[d], hi[d] = math.MaxFloat32, -math.MaxFloat32
	}
	for r := range count {
		for d := range *dims {
			x := at(r, d)
			if x < lo[d] {
				lo[d] = x
			}
			if x > hi[d] {
				hi[d] = x
			}
		}
	}

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

	row := make([]byte, *dims)
	for r := range count {
		for d := range *dims {
			span := hi[d] - lo[d]
			var q float32
			if span > 0 {
				q = (at(r, d) - lo[d]) / span * 255
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
}
