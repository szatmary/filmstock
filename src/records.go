package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// The record hierarchy is the repository. Everything else — search.db, vector
// indexes — is derived and can be deleted and rebuilt from these files without
// touching a dump.
//
//	out/movies/<shard>/<page_id>.json.gz
//	out/television/<shard>/<page_id>.json.gz
//	out/people/<shard>/<qid>.json.gz
//	out/text/<shard>/<page_id>.txt.gz     embedding corpus
//	out/manifest.jsonl                    kind, id, content hash
//
// A record's path is a pure function of its identity, never its title. Sharding
// on md5(title) meant a renamed article landed in a different file and the old
// one lingered forever as an orphan; ingest is additive, so nothing ever cleaned
// it up. Keying on page_id makes a re-extract overwrite in place.
const (
	kindMovie      = "movies"
	kindTelevision = "television"
	kindPerson     = "people"
	kindText       = "text"
	kindEvent      = "events" // award ceremonies and film festivals

	shardCount = 256
)

// recordPath returns the path for one record, relative to the output root.
// The shard is derived from the id so that identity alone determines location.
//
// The shard is taken from |id|: ids are negative for people identified only by a
// link target rather than a Q-id, and Go's % yields a negative remainder, which
// produced directories named "-1f" and 489 shards instead of 256. The sign stays
// in the filename, where it still separates the two identity spaces.
func recordPath(root, kind string, id int64, ext string) string {
	shard := id % shardCount
	if shard < 0 {
		shard = -shard
	}
	return filepath.Join(root, kind, fmt.Sprintf("%02x", shard),
		fmt.Sprintf("%d%s", id, ext))
}

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

// encodeRecordText gzips plain text (the embedding corpus) in memory.
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
	return writeBytesAtomic(recordPath(root, kind, id, ".json.gz"), data)
}

// writeRecordText writes gzipped plain text (the embedding corpus), atomically.
func writeRecordText(root string, id int64, s string) error {
	data, err := encodeRecordText(s)
	if err != nil {
		return err
	}
	return writeBytesAtomic(recordPath(root, kindText, id, ".txt.gz"), data)
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

// readRecordJSON decodes one record into v.
func readRecordJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	return json.NewDecoder(gz).Decode(v)
}

// walkRecords calls fn for every record of a kind. This is how index reads the
// repository — it never needs to know how records were produced.
func walkRecords(root, kind string, fn func(path string) error) error {
	dir := filepath.Join(root, kind)
	return filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".gz") || strings.HasSuffix(p, ".tmp") {
			return nil
		}
		return fn(p)
	})
}
