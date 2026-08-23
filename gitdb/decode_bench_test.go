package gitdb

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Where does the time go when reading a record? A line has to be base94-decoded,
// zlib-inflated against the preset dictionary, and then unmarshalled. Replacing
// JSON with protobuf or flatbuffers only addresses the third stage, so it is
// worth knowing what fraction that is before paying a codegen dependency for it.
//
// Run against the real store:
//
//	GITDB_BENCH_STORE=/tank/mediadb/filmstock-data/movies \
//	GITDB_BENCH_DICT=/tank/mediadb/filmstock-data/movies.dict \
//	go test ./gitdb -run xxx -bench Decode -benchtime 3x
func benchCorpus(tb testing.TB) (lines [][]byte, dict []byte) {
	dir := os.Getenv("GITDB_BENCH_STORE")
	if dir == "" {
		tb.Skip("set GITDB_BENCH_STORE to a store directory")
	}
	if p := os.Getenv("GITDB_BENCH_DICT"); p != "" {
		var err error
		if dict, err = os.ReadFile(p); err != nil {
			tb.Fatal(err)
		}
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*"+fileExt))
	if err != nil || len(matches) == 0 {
		tb.Skipf("no %s files in %s", fileExt, dir)
	}
	f, err := os.Open(matches[0])
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	sc.Scan() // header
	for sc.Scan() && len(lines) < 20000 {
		lines = append(lines, append([]byte(nil), sc.Bytes()...))
	}
	if len(lines) == 0 {
		tb.Skip("no records")
	}
	return lines, dict
}

func inflate(tb testing.TB, raw, dict []byte) []byte {
	var zr io.ReadCloser
	var err error
	if len(dict) > 0 {
		zr, err = zlib.NewReaderDict(bytes.NewReader(raw), dict)
	} else {
		zr, err = zlib.NewReader(bytes.NewReader(raw))
	}
	if err != nil {
		tb.Fatal(err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		tb.Fatal(err)
	}
	return out
}

// Stage 1 alone.
func BenchmarkDecodeBase94(b *testing.B) {
	lines, _ := benchCorpus(b)
	enc := Base94()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, l := range lines {
			if _, err := enc.Decode(l); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ReportMetric(float64(len(lines)), "records/op")
}

// Stages 1 + 2.
func BenchmarkDecodeBase94AndInflate(b *testing.B) {
	lines, dict := benchCorpus(b)
	enc := Base94()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, l := range lines {
			raw, err := enc.Decode(l)
			if err != nil {
				b.Fatal(err)
			}
			inflate(b, raw, dict)
		}
	}
	b.ReportMetric(float64(len(lines)), "records/op")
}

// All three stages: what a consumer actually pays per record.
func BenchmarkDecodeFull(b *testing.B) {
	lines, dict := benchCorpus(b)
	enc := Base94()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, l := range lines {
			raw, err := enc.Decode(l)
			if err != nil {
				b.Fatal(err)
			}
			var v map[string]any
			if err := json.Unmarshal(inflate(b, raw, dict), &v); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ReportMetric(float64(len(lines)), "records/op")
}

// Stage 3 alone, from already-inflated JSON: the only part a protobuf or
// flatbuffers migration could make faster.
func BenchmarkJSONUnmarshalOnly(b *testing.B) {
	lines, dict := benchCorpus(b)
	enc := Base94()
	docs := make([][]byte, 0, len(lines))
	for _, l := range lines {
		raw, err := enc.Decode(l)
		if err != nil {
			b.Fatal(err)
		}
		docs = append(docs, inflate(b, raw, dict))
	}
	var total int
	for _, d := range docs {
		total += len(d)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, d := range docs {
			var v map[string]any
			if err := json.Unmarshal(d, &v); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ReportMetric(float64(len(docs)), "records/op")
	b.ReportMetric(float64(total)/float64(len(docs)), "bytes/record")
}

// Scanning lines WITHOUT decoding anything. This is what building an identity
// map costs once the key and version are a plaintext prefix on each line — the
// walk a consumer does after `git pull`.
func BenchmarkScanLinesOnly(b *testing.B) {
	lines, _ := benchCorpus(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var n int
		for _, l := range lines {
			if j := bytes.IndexByte(l, ' '); j > 0 {
				n += j
			}
		}
		_ = n
	}
	b.ReportMetric(float64(len(lines)), "records/op")
}
