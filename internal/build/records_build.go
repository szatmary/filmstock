package build

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/szatmary/filmstock"
)

// encodeRecordJSON serialises v to gzipped JSON in memory.
//
// Compression runs on the caller — a parser worker with CPU to spare — so the
// writer threads do nothing but syscalls. On a pool that can only sustain a few
// hundred IOPS, every millisecond of CPU held on a writer is a millisecond the
// queue is not draining.
func encodeRecordJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	enc := json.NewEncoder(gz)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// encodeRecordText gzips plain text (the full-text corpus) in memory.
func encodeRecordText(s string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := io.WriteString(gz, s); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeBytesAtomic writes data to path via a temp file and rename, so a crash
// mid-write cannot leave a truncated record that a later index run would
// silently accept as complete.
func writeBytesAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// writeRecordJSON writes v as gzipped JSON, atomically.
func writeRecordJSON(root, kind string, id int64, v any) error {
	data, err := encodeRecordJSON(v)
	if err != nil {
		return err
	}
	return writeBytesAtomic(filmstock.RecordPath(root, kind, id, ".json.gz"), data)
}

// writeRecordText writes gzipped plain text (the full-text corpus), atomically.
func writeRecordText(root string, id int64, s string) error {
	data, err := encodeRecordText(s)
	if err != nil {
		return err
	}
	return writeBytesAtomic(filmstock.RecordPath(root, filmstock.KindText, id, ".txt.gz"), data)
}

// pendingWrite is one already-compressed record waiting for a writer thread.
type pendingWrite struct {
	path string
	data []byte
}

// writePool drains encoded records to disk on dedicated threads.
//
// Parser workers used to call straight into the filesystem, so all 18 of them
// sat in iowait behind a pool that sustains a few hundred IOPS — 700% CPU out of
// a possible 2000%. Handing finished bytes to a queue lets the parsers run ahead
// while the writers absorb the latency.
type writePool struct {
	q    chan pendingWrite
	wg   sync.WaitGroup
	mu   sync.Mutex
	err  error
	n    int64
	skip int64
}

func newWritePool(threads, depth int) *writePool {
	w := &writePool{q: make(chan pendingWrite, depth)}
	for i := 0; i < threads; i++ {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			for p := range w.q {
				if err := writeBytesAtomic(p.path, p.data); err != nil {
					w.fail(err)
				} else {
					atomic.AddInt64(&w.n, 1)
				}
			}
		}()
	}
	return w
}

func (w *writePool) fail(e error) {
	w.mu.Lock()
	if w.err == nil {
		w.err = e
	}
	w.mu.Unlock()
}

func (w *writePool) put(path string, data []byte) {
	w.q <- pendingWrite{path: path, data: data}
}

// close drains the queue and waits for every write to land.
func (w *writePool) close() error {
	close(w.q)
	w.wg.Wait()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}
