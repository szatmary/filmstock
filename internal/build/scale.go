package build

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
)

// The per-dimension quantisation range, pinned to an epoch and stored beside the
// vectors rather than recomputed from whatever happens to be in the corpus.
//
// # Why it is frozen
//
// lo/hi used to be the corpus-wide min and max, recomputed on every build. That
// makes the file undistributable: one new film that sets a new extremum on a
// dimension shifts that dimension's span, and every row's byte in it changes.
// Measured on the 170,421-row film corpus, growing it by 10% moved 15.66% of the
// bytes in rows that had not themselves changed. Scattered that thinly, every
// 4 KiB block in the file is dirty, so a block-level diff transfers the whole
// 175 MB to deliver ~2 MB of new rows.
//
// Freezing the range costs almost nothing. Against float32 ground truth, top-20
// neighbour overlap is 0.9911 with the range recomputed over all rows and 0.9908
// with it frozen on the first 90% and the remaining 10% clamped into it — 0.0003,
// for a corpus grown by 17,043 films, and 0.0001% of values clamped.
//
// Min and max, not percentiles. Clipping at the 0.1/99.9 percentile scores 0.9893
// and at 0.5/99.5 scores 0.9774: at int8 there is resolution to spare, so the
// precision bought by a tighter range does not pay for what clipping loses. That
// reverses at int4, where levels are scarce enough that 0.5/99.5 (0.9248) beats
// min/max (0.8797) — so a narrower range is a decision that belongs to a bit
// depth, and this file ships int8.
type Scale struct {
	Dims int
	Lo   []float32
	Hi   []float32
}

const scaleMagic = "fsscale1\n"

// ID is the identity of a range, so a build that extends an existing vectors.bin
// can prove it is quantising against the same numbers the existing rows used.
func (s *Scale) ID() string {
	b := make([]byte, 0, len(s.Lo)*8)
	for d := range s.Lo {
		b = binary.LittleEndian.AppendUint32(b, math.Float32bits(s.Lo[d]))
		b = binary.LittleEndian.AppendUint32(b, math.Float32bits(s.Hi[d]))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// DeriveScale takes the per-dimension min and max over every row. This starts an
// epoch; it is not what a routine build does.
func DeriveScale(at func(r, d int) float32, count, dims int) *Scale {
	s := &Scale{Dims: dims, Lo: make([]float32, dims), Hi: make([]float32, dims)}
	for d := range dims {
		s.Lo[d], s.Hi[d] = math.MaxFloat32, -math.MaxFloat32
	}
	for r := range count {
		for d := range dims {
			x := at(r, d)
			if x < s.Lo[d] {
				s.Lo[d] = x
			}
			if x > s.Hi[d] {
				s.Hi[d] = x
			}
		}
	}
	return s
}

func LoadScale(path string) (*Scale, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) < len(scaleMagic)+4 || string(b[:len(scaleMagic)]) != scaleMagic {
		return nil, fmt.Errorf("%s: not a filmstock scale file", path)
	}
	dims := int(binary.LittleEndian.Uint32(b[len(scaleMagic):]))
	off := len(scaleMagic) + 4
	if dims <= 0 || len(b) != off+dims*8 {
		return nil, fmt.Errorf("%s: %d bytes for %d dims, want %d", path, len(b), dims, off+dims*8)
	}
	s := &Scale{Dims: dims, Lo: make([]float32, dims), Hi: make([]float32, dims)}
	for d := range dims {
		s.Lo[d] = math.Float32frombits(binary.LittleEndian.Uint32(b[off+d*8:]))
		s.Hi[d] = math.Float32frombits(binary.LittleEndian.Uint32(b[off+d*8+4:]))
	}
	return s, nil
}

func (s *Scale) Write(path string) error {
	b := make([]byte, 0, len(scaleMagic)+4+s.Dims*8)
	b = append(b, scaleMagic...)
	b = binary.LittleEndian.AppendUint32(b, uint32(s.Dims))
	for d := range s.Dims {
		b = binary.LittleEndian.AppendUint32(b, math.Float32bits(s.Lo[d]))
		b = binary.LittleEndian.AppendUint32(b, math.Float32bits(s.Hi[d]))
	}
	return os.WriteFile(path, b, 0o644)
}

// ErrNoScale is returned when -scale names a file that is not there. Deriving one
// silently would start a new epoch and rewrite every byte of every vector file
// built against the old one, which is exactly the thing this type exists to stop.
var ErrNoScale = errors.New("scale file does not exist")
