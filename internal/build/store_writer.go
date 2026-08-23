package build

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/szatmary/filmstock"
	"github.com/szatmary/gitdb"
)

// storeWriter writes records into the per-kind gitdb stores, keeping each
// record's identity (page_id, or Q-id for a person) pointed at the store id that
// holds it.
//
// The mapping is rebuilt by reading the existing store rather than kept in a
// side file, so the store stays self-describing: every record already carries
// its own identity, and a mapping that lived elsewhere could go stale against
// it. That read is also what makes a re-extract an Update of the existing record
// rather than a second copy under a new id — ids are permanent, and a consumer
// holding one must keep getting the same work back.
type storeWriter struct {
	mu     sync.Mutex
	stores map[string]*gitdb.DB
	byID   map[string]map[int64]uint64 // kind -> identity -> store id
	root   string
	err    error
}

func newStoreWriter(root string) *storeWriter {
	return &storeWriter{
		root:   root,
		stores: map[string]*gitdb.DB{},
		byID:   map[string]map[int64]uint64{},
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

// open loads a kind's store and the identity mapping it already contains.
func (w *storeWriter) open(kind string) (*gitdb.DB, map[int64]uint64, error) {
	if d, ok := w.stores[kind]; ok {
		return d, w.byID[kind], nil
	}
	d, err := filmstock.OpenStore(w.root, kind)
	if err != nil {
		return nil, nil, err
	}
	m := map[int64]uint64{}
	for rec, err := range d.All() {
		if err != nil {
			return nil, nil, fmt.Errorf("scanning existing %s store: %w", kind, err)
		}
		if id, ok := identityOf(kind, rec.Data); ok {
			m[id] = rec.ID
		}
	}
	w.stores[kind], w.byID[kind] = d, m
	return d, m, nil
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
	d, m, err := w.open(kind)
	if err != nil {
		w.err = err
		return
	}
	if existing, ok := m[identity]; ok {
		if err := d.Update(existing, data); err != nil {
			w.err = fmt.Errorf("%s %d: %w", kind, identity, err)
		}
		return
	}
	id, err := d.Insert(data)
	if err != nil {
		w.err = fmt.Errorf("%s %d: %w", kind, identity, err)
		return
	}
	m[identity] = id
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
	for kind, m := range w.byID {
		out[kind] = len(m)
	}
	return out
}
