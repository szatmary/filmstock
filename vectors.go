package filmstock

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"sort"
)

// Embedding vectors, one per record, for similarity and for navigating the
// space they span.
//
// # The file
//
// One file, mmap-friendly, laid out so a reader can seek straight to a row:
//
//	magic   "fsvec1\n"
//	count   uint32   rows
//	dims    uint32   values per row
//	scale   dims × (float32 lo, float32 hi)   per-dimension dequantisation
//	ids     count × int32                     page_id of each row
//	data    count × dims × uint8              quantised values
//
// Values are int8 with a per-DIMENSION scale, which is what makes the size
// honest: a single global scale crushes low-variance dimensions and loses about
// half the near neighbours, while per-dimension scaling keeps 99.4% of the top
// 20 at a quarter of the float32 size. Measured on 20,000 films: float32 666 MB,
// int8 166 MB at 0.994 neighbour overlap. int4 gives 83 MB at 0.905, and int2
// collapses to 0.597 — the useful floor is int8.
const (
	vecMagic   = "fsvec1\n"
	vecHdrSize = len(vecMagic) + 8
)

// Vectors is an opened embedding file.
type Vectors struct {
	dims  int
	ids   []int32
	lo    []float32 // per-dimension dequantisation, length dims
	span  []float32 // (hi-lo)/255, length dims
	data  []uint8   // count × dims
	byID  map[int32]int32
	unit  [][]float32 // lazily materialised unit rows
	cache bool
}

// A Neighbour is one result of a similarity query.
type Neighbour struct {
	PageID int
	Score  float32 // cosine similarity, 1 is identical
}

// OpenVectors reads an embedding file into memory. The whole corpus is 166 MB
// at int8, small enough that memory-mapping and paging is not worth the
// complexity of the failure modes it introduces.
func OpenVectors(path string) (*Vectors, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("filmstock: vectors: %w", err)
	}
	if len(b) < vecHdrSize || string(b[:len(vecMagic)]) != vecMagic {
		return nil, fmt.Errorf("filmstock: %s: not a filmstock vector file", path)
	}
	off := len(vecMagic)
	count := int(binary.LittleEndian.Uint32(b[off:]))
	dims := int(binary.LittleEndian.Uint32(b[off+4:]))
	off += 8
	if count <= 0 || dims <= 0 {
		return nil, fmt.Errorf("filmstock: %s: empty vector file", path)
	}
	need := off + dims*8 + count*4 + count*dims
	if len(b) != need {
		return nil, fmt.Errorf("filmstock: %s: file is %d bytes, header describes %d",
			path, len(b), need)
	}

	v := &Vectors{
		dims: dims,
		lo:   make([]float32, dims),
		span: make([]float32, dims),
		ids:  make([]int32, count),
		byID: make(map[int32]int32, count),
	}
	for d := range dims {
		lo := math.Float32frombits(binary.LittleEndian.Uint32(b[off:]))
		hi := math.Float32frombits(binary.LittleEndian.Uint32(b[off+4:]))
		v.lo[d], v.span[d] = lo, (hi-lo)/255
		off += 8
	}
	for i := range count {
		v.ids[i] = int32(binary.LittleEndian.Uint32(b[off:]))
		v.byID[v.ids[i]] = int32(i)
		off += 4
	}
	v.data = b[off:]
	return v, nil
}

// Len reports how many records have a vector.
func (v *Vectors) Len() int { return len(v.ids) }

// Dims reports the vector width.
func (v *Vectors) Dims() int { return v.dims }

// Has reports whether a record has a vector.
func (v *Vectors) Has(pageID int) bool { _, ok := v.byID[int32(pageID)]; return ok }

// IDs returns every page_id that has a vector, in file order.
func (v *Vectors) IDs() []int32 { return v.ids }

// row dequantises one row into a unit vector.
func (v *Vectors) row(i int32) []float32 {
	out := make([]float32, v.dims)
	base := int(i) * v.dims
	var sum float64
	for d := range v.dims {
		x := v.lo[d] + float32(v.data[base+d])*v.span[d]
		out[d] = x
		sum += float64(x) * float64(x)
	}
	if n := float32(math.Sqrt(sum)); n > 0 {
		for d := range out {
			out[d] /= n
		}
	}
	return out
}

// Vector returns the unit embedding for a record.
func (v *Vectors) Vector(pageID int) ([]float32, bool) {
	i, ok := v.byID[int32(pageID)]
	if !ok {
		return nil, false
	}
	return v.row(i), true
}

// Similar returns the n records closest to pageID, nearest first, excluding
// pageID itself.
//
// Brute force over every row. At 170k × 1024 that is a few hundred milliseconds
// and needs no index; against a collection it is sub-millisecond. An approximate
// index would add a dependency and a recall cliff to save time nobody is waiting
// for.
func (v *Vectors) Similar(pageID, n int) ([]Neighbour, error) {
	q, ok := v.Vector(pageID)
	if !ok {
		return nil, fmt.Errorf("filmstock: no vector for %d: %w", pageID, ErrNotFound)
	}
	return v.nearest(q, n, map[int]bool{pageID: true}, nil), nil
}

// nearest scores every candidate row against q. When only is non-nil, scoring is
// restricted to those rows.
func (v *Vectors) nearest(q []float32, n int, skip map[int]bool, only []int32) []Neighbour {
	rows := only
	if rows == nil {
		rows = make([]int32, len(v.ids))
		for i := range v.ids {
			rows[i] = int32(i)
		}
	}
	out := make([]Neighbour, 0, len(rows))
	for _, i := range rows {
		id := int(v.ids[i])
		if skip[id] {
			continue
		}
		base := int(i) * v.dims
		var dot, norm float64
		for d := range v.dims {
			x := float64(v.lo[d] + float32(v.data[base+d])*v.span[d])
			dot += float64(q[d]) * x
			norm += x * x
		}
		if norm > 0 {
			dot /= math.Sqrt(norm)
		}
		out = append(out, Neighbour{PageID: id, Score: float32(dot)})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Score > out[b].Score })
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}
