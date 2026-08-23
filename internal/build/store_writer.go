package build

import (
	"bytes"
	"database/sql"
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
	mu        sync.Mutex
	stores    map[string]*gitdb.DB
	byID      map[string]map[int64]uint64 // kind -> identity -> store id
	byWiki    map[string]int64            // person article title -> identity
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

// loadIdentitiesFromIndex fills the identity maps from the search index instead
// of scanning the store.
//
// Scanning is right for a full extract, which is going to read everything
// anyway. It is badly wrong for an incremental pass: inflating all 450,701
// records against the dictionary costs ~23 seconds before the first page of a
// day's changes is even looked at, to apply about 145 film edits. The index
// already holds page_id -> gitdb_id for every kind, which is precisely this
// mapping, and reading it is four queries.
func (w *storeWriter) loadIdentitiesFromIndex(db *sql.DB) error {
	for _, q := range []struct{ kind, sql string }{
		{filmstock.KindMovie, `SELECT id, gitdb_id FROM movies`},
		{filmstock.KindTelevision, `SELECT id, gitdb_id FROM television_series`},
		{filmstock.KindEvent, `SELECT id, gitdb_id FROM events`},
		{filmstock.KindPerson, `SELECT COALESCE(qid, -1), gitdb_id, COALESCE(wiki,'') FROM people WHERE gitdb_id IS NOT NULL`},
	} {
		rows, err := db.Query(q.sql)
		if err != nil {
			return fmt.Errorf("reading %s ids from the index: %w", q.kind, err)
		}
		m := map[int64]uint64{}
		for rows.Next() {
			var identity int64
			var gid uint64
			if q.kind == filmstock.KindPerson {
				var wiki string
				if err := rows.Scan(&identity, &gid, &wiki); err != nil {
					rows.Close()
					return err
				}
				// A person with no Q-id is keyed on a hash of their link target,
				// exactly as the extractor keyed them.
				if identity < 0 && wiki != "" {
					identity = -int64(filmstock.PersonRecordPathID(wiki))
				}
				if wiki != "" {
					w.byWiki[wiki] = identity
				}
			} else if err := rows.Scan(&identity, &gid); err != nil {
				rows.Close()
				return err
			}
			m[identity] = gid
		}
		rows.Close()
		d, err := filmstock.OpenStore(w.root, q.kind)
		if err != nil {
			return err
		}
		w.stores[q.kind], w.byID[q.kind] = d, m
	}
	return nil
}

func newStoreWriter(root string) *storeWriter {
	return &storeWriter{
		root:   root,
		stores: map[string]*gitdb.DB{},
		byID:   map[string]map[int64]uint64{},
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
		id, ok := identityOf(kind, rec.Data)
		if !ok {
			continue
		}
		m[id] = rec.ID
		if kind == filmstock.KindPerson {
			// An incremental pass meets a biography by article title and has to
			// find the person it belongs to. Identity is a Q-id, which the title
			// alone does not give, so the mapping is read from the store.
			var probe struct {
				Wiki string `json:"wiki"`
			}
			if json.Unmarshal(rec.Data, &probe) == nil && probe.Wiki != "" {
				w.byWiki[probe.Wiki] = id
			}
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
		// Only write when the bytes actually changed. gitdb's Update appends a
		// new version and rewrites the index entry regardless of content, so
		// writing unconditionally makes a re-ingest of an unchanged dump rewrite
		// the entire store — measured at 450,633 deleted lines and every index
		// shard churned, for zero real change. The comparison is what makes an
		// incremental ingest incremental.
		if cur, err := d.Get(existing); err == nil && bytes.Equal(cur, data) {
			w.unchanged++
			return
		}
		if err := d.Update(existing, data); err != nil {
			w.err = fmt.Errorf("%s %d: %w", kind, identity, err)
		}
		w.updated++
		return
	}
	id, err := d.Insert(data)
	if err != nil {
		w.err = fmt.Errorf("%s %d: %w", kind, identity, err)
		return
	}
	m[identity] = id
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
	d, m, err := w.open(kind)
	if err != nil {
		w.err = err
		return false
	}
	id, ok := m[identity]
	if !ok {
		return false
	}
	if err := d.Delete(id); err != nil {
		w.err = fmt.Errorf("delete %s %d: %w", kind, identity, err)
		return false
	}
	delete(m, identity)
	return true
}

// get returns a record's current bytes, or nil when there is none.
func (w *storeWriter) get(kind string, identity int64) []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	d, m, err := w.open(kind)
	if err != nil {
		w.err = err
		return nil
	}
	id, ok := m[identity]
	if !ok {
		return nil
	}
	b, err := d.Get(id)
	if err != nil {
		return nil
	}
	return b
}

// personIdentityFor resolves a person's article title to their identity.
func (w *storeWriter) personIdentityFor(wiki string) (int64, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, _, err := w.open(filmstock.KindPerson); err != nil {
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
	for kind, m := range w.byID {
		out[kind] = len(m)
	}
	return out
}
