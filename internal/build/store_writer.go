package build

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/szatmary/filmstock"
	"github.com/szatmary/filmstock/gitdb"
)

// storeWriter writes records into the per-kind gitdb stores.
//
// Records are keyed by their own identity — page_id for a work, Q-id for a
// person — so writing one is a Put under that key and there is no mapping to
// maintain, rebuild, or let go stale. A re-extract therefore updates the record
// that is already there rather than making a second copy, by construction rather
// than by bookkeeping.
// A recordSink takes finished records. The record builder needs three things
// from it and no more, which is what makes the storage format replaceable: the
// gitdb tree and the published SQLite database are both just sinks.
type recordSink interface {
	// put stores one record under its identity, which is always an enwiki
	// page_id.
	put(kind string, identity int64, v any)
	// sweep removes whatever a complete run did not write, and reports how
	// many. A partial run must not call it.
	sweep() int
	// Counts reports what the run left alone, so an ingest can say how much of
	// the store it did not touch.
	Counts() (unchanged, updated, inserted int)
	// Err reports the first failure, if any.
	Err() error
}

type storeWriter struct {
	mu     sync.Mutex
	stores map[string]*gitdb.DB
	root   string
	err    error
	// wrote records every key the run produced, per kind, so a complete export
	// can remove the ones it did not. Nil disables the sweep, which is what a
	// partial run wants.
	wrote     map[string]map[string]bool
	unchanged int
	updated   int
	inserted  int
}

// Counts reports what the last run actually wrote, so an ingest can say how much
// of the store it left alone rather than implying it rewrote everything.
func (w *storeWriter) Counts() (unchanged, updated, inserted int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.unchanged, w.updated, w.inserted
}

func newStoreWriter(root string) *storeWriter {
	return &storeWriter{
		root:   root,
		stores: map[string]*gitdb.DB{},
	}
}

// identityOf reads the identity out of a record's own bytes. Works records carry
// page_id; people carry qid, and fall back to a hash of their link target when
// they have none — the same rule the record itself was keyed on.
func identityOf(kind string, data []byte) (int64, bool) {
	var probe struct {
		PageID int64  `json:"page_id"`
		QID    int64  `json:"qid"`
		Wiki   string `json:"wiki"`
	}
	if json.Unmarshal(data, &probe) != nil {
		return 0, false
	}
	// Every kind, people included, is keyed by page_id. A credit whose link
	// target has no article has none, and so has no record.
	return probe.PageID, probe.PageID != 0
}

// open loads a kind's store.
//
// Opening scans the record files to build gitdb's key map, which is cheap: it
// reads each line's key and version prefix without decoding payloads. Only the
// people store needs more than that — a biography arrives by article title and
// the title alone does not give a Q-id — so only that one pays for a full decode.
func (w *storeWriter) open(kind string) (*gitdb.DB, error) {
	if d, ok := w.stores[kind]; ok {
		return d, nil
	}
	d, err := filmstock.OpenStore(w.root, kind)
	if err != nil {
		return nil, err
	}
	w.stores[kind] = d
	return d, nil
}

// put writes one record, updating in place when this identity is already stored.
func (w *storeWriter) put(kind string, identity int64, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		w.fail(err)
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return
	}
	d, err := w.open(kind)
	if err != nil {
		w.err = err
		return
	}
	key := filmstock.StoreKey(identity)
	if w.wrote != nil {
		seen := w.wrote[kind]
		if seen == nil {
			seen = map[string]bool{}
			w.wrote[kind] = seen
		}
		seen[key] = true
	}
	if d.Has(key) {
		// Only write when the bytes actually changed. A Put appends a new line
		// regardless of content, so writing unconditionally makes a re-ingest of
		// an unchanged dump rewrite the entire store — measured at 450,633
		// rewritten records for zero real change. On a normal day this check
		// suppresses about 4,400 writes and permits about 590: roughly seven in
		// eight changed pages re-derive to identical bytes.
		if cur, err := d.Get(key); err == nil && bytes.Equal(cur, data) {
			w.unchanged++
			return
		}
		if err := d.Put(key, data); err != nil {
			w.err = fmt.Errorf("%s %d: %w", kind, identity, err)
		}
		w.updated++
		return
	}
	if err := d.Put(key, data); err != nil {
		w.err = fmt.Errorf("%s %d: %w", kind, identity, err)
		return
	}
	w.inserted++
}

// sweep removes every record the run did not produce.
//
// An export derives the COMPLETE set of records from the intermediate, so a key
// that survives in the store without being written this time is a record the
// encyclopaedia no longer supports: a page whose infobox was removed, a person
// whose article appeared so their credit now resolves to a page_id and their
// old hash-keyed record is orphaned, a film that turned out to be a
// disambiguation page.
//
// Without this an export into an EXISTING store could only ever add. A fresh
// export into an empty directory looked correct because the stale records
// simply were not written; running the same export over yesterday's store kept
// every one of them, and nothing said so.
//
// Only safe for a complete export. A partial run has not written the records it
// did not reach, and sweeping would delete the entire rest of the store.
func (w *storeWriter) sweep() (removed int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil || w.wrote == nil {
		return 0
	}
	for kind, d := range w.stores {
		seen := w.wrote[kind]
		for _, key := range d.Keys() {
			if seen[key] {
				continue
			}
			if err := d.Delete(key); err != nil {
				w.err = fmt.Errorf("sweep %s %s: %w", kind, key, err)
				return removed
			}
			removed++
		}
	}
	return removed
}

func (w *storeWriter) fail(e error) {
	w.mu.Lock()
	if w.err == nil {
		w.err = e
	}
	w.mu.Unlock()
}

func (w *storeWriter) Err() error { return w.err }

// counts reports how many records each opened store holds, for the run summary.
func (w *storeWriter) counts() map[string]int {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[string]int{}
	for kind, d := range w.stores {
		out[kind] = d.Len()
	}
	return out
}
