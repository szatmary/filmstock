package query

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// writeVectors builds a vector file, so the tests exercise the real format
// rather than a hand-made struct.
func writeVectors(t *testing.T, ids []int32, rows [][]float32) string {
	t.Helper()
	dims := len(rows[0])
	lo := make([]float32, dims)
	hi := make([]float32, dims)
	for d := range dims {
		lo[d], hi[d] = math.MaxFloat32, -math.MaxFloat32
		for _, r := range rows {
			lo[d] = min(lo[d], r[d])
			hi[d] = max(hi[d], r[d])
		}
	}
	var b []byte
	b = append(b, vecMagic...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(rows)))
	b = binary.LittleEndian.AppendUint32(b, uint32(dims))
	for d := range dims {
		b = binary.LittleEndian.AppendUint32(b, math.Float32bits(lo[d]))
		b = binary.LittleEndian.AppendUint32(b, math.Float32bits(hi[d]))
	}
	for _, id := range ids {
		b = binary.LittleEndian.AppendUint32(b, uint32(id))
	}
	for _, r := range rows {
		for d := range dims {
			var q float32
			if hi[d] > lo[d] {
				q = (r[d] - lo[d]) / (hi[d] - lo[d]) * 255
			}
			b = append(b, uint8(math.Round(float64(min(max(q, 0), 255)))))
		}
	}
	p := filepath.Join(t.TempDir(), "v.bin")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestVectorsRoundTrip(t *testing.T) {
	ids := []int32{10, 20, 30}
	rows := [][]float32{{1, 0, 0}, {0, 1, 0}, {0.9, 0.1, 0}}
	v, err := OpenVectors(writeVectors(t, ids, rows))
	if err != nil {
		t.Fatal(err)
	}
	if v.Len() != 3 || v.Dims() != 3 {
		t.Fatalf("got %d rows x %d dims", v.Len(), v.Dims())
	}
	if !v.Has(20) || v.Has(99) {
		t.Error("Has is wrong")
	}
	got, ok := v.Vector(10)
	if !ok {
		t.Fatal("no vector for 10")
	}
	if math.Abs(float64(dot(got, got))-1) > 1e-4 {
		t.Errorf("returned vector is not unit length: %v", got)
	}
}

// 30 must come back before 20: it is nearly parallel to 10.
func TestSimilarOrdersByCosine(t *testing.T) {
	v, err := OpenVectors(writeVectors(t,
		[]int32{10, 20, 30}, [][]float32{{1, 0, 0}, {0, 1, 0}, {0.9, 0.1, 0}}))
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.Similar(10, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].PageID != 30 {
		t.Fatalf("got %+v, want 30 first", got)
	}
	for _, n := range got {
		if n.PageID == 10 {
			t.Error("Similar returned the query itself")
		}
	}
}

func TestSimilarUnknownID(t *testing.T) {
	v, _ := OpenVectors(writeVectors(t, []int32{1}, [][]float32{{1, 0}}))
	if _, err := v.Similar(999, 5); err == nil {
		t.Error("expected an error for a record with no vector")
	}
}

func TestOpenVectorsRejectsTruncatedFile(t *testing.T) {
	p := writeVectors(t, []int32{1, 2}, [][]float32{{1, 0}, {0, 1}})
	b, _ := os.ReadFile(p)
	short := filepath.Join(t.TempDir(), "short.bin")
	os.WriteFile(short, b[:len(b)-3], 0o644)
	if _, err := OpenVectors(short); err == nil {
		t.Error("a truncated file must not open: rows would be misaligned and every result silently wrong")
	}
}

func TestOpenVectorsRejectsForeignFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nope.bin")
	os.WriteFile(p, []byte("this is not a vector file at all"), 0o644)
	if _, err := OpenVectors(p); err == nil {
		t.Error("expected a magic-number rejection")
	}
}

// Collect must report what it could not find, so a caller can say "412 of your
// 600 films are covered" instead of silently exploring a subset.
func TestCollectReportsMissing(t *testing.T) {
	v, _ := OpenVectors(writeVectors(t,
		[]int32{1, 2, 3}, [][]float32{{1, 0}, {0, 1}, {1, 1}}))
	c, missing := v.Collect([]int{1, 3, 999, 1000})
	if c.Len() != 2 {
		t.Errorf("collection has %d, want 2", c.Len())
	}
	if missing != 2 {
		t.Errorf("missing = %d, want 2", missing)
	}
}

func TestCollectDeduplicates(t *testing.T) {
	v, _ := OpenVectors(writeVectors(t, []int32{1, 2}, [][]float32{{1, 0}, {0, 1}}))
	c, _ := v.Collect([]int{1, 1, 1, 2})
	if c.Len() != 2 {
		t.Errorf("collection has %d, want 2 — duplicates must collapse", c.Len())
	}
}
