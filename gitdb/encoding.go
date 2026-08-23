package gitdb

import "fmt"

// Encoding maps arbitrary binary data onto a restricted byte alphabet that is
// safe to store one-per-line in a text file, and back. Every encoding's output
// is guaranteed to be free of NUL, CR, and LF.
type Encoding interface {
	// Name identifies the encoding in file headers.
	Name() string
	// Encode returns the line-safe representation of src.
	Encode(src []byte) []byte
	// Decode inverts Encode. It returns an error if src is not a valid
	// encoding output.
	Decode(src []byte) ([]byte, error)
}

// Base94 encodes 4 raw bytes per 5 output bytes (25% expansion) using only the
// visible ASCII characters 0x21..0x7E. The most conservative choice: output is
// copy-paste safe and renders cleanly everywhere.
func Base94() Encoding { return encBase94 }

// EncodingByName returns the encoding a file header names.
func EncodingByName(name string) (Encoding, error) {
	for _, e := range []Encoding{encBase94} {
		if e.Name() == name {
			return e, nil
		}
	}
	return nil, fmt.Errorf("gitdb: unknown encoding %q", name)
}

// base94 is the only encoding gitdb ships. Base118 (denser) and Escape253
// (densest) were removed as unused: the store has always been base94, whose
// output is plain visible ASCII and therefore safe to paste, diff, and view in
// a browser. A denser alphabet saves bytes before compression, not after.
var encBase94 = newBlockCode("base94", 4, base94Alphabet())

func base94Alphabet() []byte {
	var a []byte
	for b := byte(0x21); b <= 0x7E; b++ {
		a = append(a, b)
	}
	return a
}

// blockCode is a radix code over an arbitrary alphabet: every group of k raw
// bytes (1 <= k <= rawLen) becomes k+1 output characters, interpreted as a
// big-endian base-len(alphabet) number. This requires len(alphabet)^(k+1) >=
// 256^k for every k, which holds for base >= 116 at rawLen 6 and base >= 88
// at rawLen 4.
type blockCode struct {
	name   string
	rawLen int
	alpha  []byte
	rev    [256]int16 // alphabet byte -> digit value, -1 if not in alphabet
}

func newBlockCode(name string, rawLen int, alphabet []byte) *blockCode {
	c := &blockCode{name: name, rawLen: rawLen, alpha: alphabet}
	for i := range c.rev {
		c.rev[i] = -1
	}
	for i, b := range alphabet {
		c.rev[b] = int16(i)
	}
	return c
}

func (c *blockCode) Name() string { return c.name }

func (c *blockCode) Encode(src []byte) []byte {
	base := uint64(len(c.alpha))
	out := make([]byte, 0, (len(src)/c.rawLen+1)*(c.rawLen+1))
	for len(src) > 0 {
		k := min(len(src), c.rawLen)
		var v uint64
		for _, b := range src[:k] {
			v = v<<8 | uint64(b)
		}
		src = src[k:]
		var group [8]byte
		for i := k; i >= 0; i-- {
			group[i] = c.alpha[v%base]
			v /= base
		}
		out = append(out, group[:k+1]...)
	}
	return out
}

func (c *blockCode) Decode(src []byte) ([]byte, error) {
	encLen := c.rawLen + 1
	if len(src)%encLen == 1 {
		return nil, fmt.Errorf("gitdb: %s: invalid input length %d", c.name, len(src))
	}
	base := uint64(len(c.alpha))
	out := make([]byte, 0, len(src)/encLen*c.rawLen+c.rawLen)
	for len(src) > 0 {
		m := min(len(src), encLen)
		var v uint64
		for _, b := range src[:m] {
			d := c.rev[b]
			if d < 0 {
				return nil, fmt.Errorf("gitdb: %s: invalid byte 0x%02X", c.name, b)
			}
			v = v*base + uint64(d)
		}
		src = src[m:]
		n := m - 1
		if v >= 1<<(8*n) {
			return nil, fmt.Errorf("gitdb: %s: group value out of range", c.name)
		}
		for i := n - 1; i >= 0; i-- {
			out = append(out, byte(v>>(8*i)))
		}
	}
	return out, nil
}
