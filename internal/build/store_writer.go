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
type storeWriter struct {
	mu     sync.Mutex
	stores map[string]*gitdb.DB
	// byWiki maps a person's article title to their identity. This is real
	// information the store holds and a title alone cannot give, unlike the
	// identity -> store-id map that used to sit beside it: format 6 keys records
	// by identity, so that one no longer exists.
	byWiki    map[string]int64
	root      string
	err       error
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
		byWiki: map[string]int64{},
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
	if kind == filmstock.KindPerson {
		if probe.QID != 0 {
			return probe.QID, true
		}
		if probe.Wiki != "" {
			return -int64(filmstock.PersonRecordPathID(probe.Wiki)), true
		}
		return 0, false
	}
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
	if kind == filmstock.KindPerson {
		for rec, err := range d.All() {
			if err != nil {
				return nil, fmt.Errorf("scanning existing %s store: %w", kind, err)
			}
			id, ok := identityOf(kind, rec.Data)
			if !ok {
				continue
			}
			var probe struct {
				Wiki string `json:"wiki"`
			}
			if json.Unmarshal(rec.Data, &probe) == nil && probe.Wiki != "" {
				w.byWiki[probe.Wiki] = id
			}
		}
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

// delete removes a record. Used when a page stops qualifying — an infobox is
// removed and a film is no longer a film. gitdb clears the index entry and
// leaves the bytes, so this is one changed line and no other record moves.
//
// Returns whether anything was there to remove.
func (w *storeWriter) delete(kind string, identity int64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return false
	}
	d, err := w.open(kind)
	if err != nil {
		w.err = err
		return false
	}
	key := filmstock.StoreKey(identity)
	if !d.Has(key) {
		return false
	}
	if err := d.Delete(key); err != nil {
		w.err = fmt.Errorf("delete %s %d: %w", kind, identity, err)
		return false
	}
	return true
}

// get returns a record's current bytes, or nil when there is none.
func (w *storeWriter) get(kind string, identity int64) []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	d, err := w.open(kind)
	if err != nil {
		w.err = err
		return nil
	}
	b, err := d.Get(filmstock.StoreKey(identity))
	if err != nil {
		return nil
	}
	return b
}

// personIdentityFor resolves a person's article title to their identity.
func (w *storeWriter) personIdentityFor(wiki string) (int64, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.open(filmstock.KindPerson); err != nil {
		w.err = err
		return 0, false
	}
	id, ok := w.byWiki[wiki]
	return id, ok
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
