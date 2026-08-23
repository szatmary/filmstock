package filmstock

import (
	"context"
	_ "embed"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/szatmary/gitdb"
)

// dictionary is a zlib preset dictionary trained on a sample of the corpus.
//
// It exists because gitdb compresses every record independently, and these
// records are small — a person is a few hundred bytes — so on its own a record
// has almost nothing to match against. Trained on 15,166 sample records it buys
// 21.7% over plain zlib on held-out data, and 32.9% on people, which are the
// smallest and most repetitive. That gain is very nearly the 25% Base94 costs to
// encode, which is what makes storing these in a text file affordable.
//
// It is embedded rather than loaded from disk because a store records its
// dictionary's identity in its header: opening a store with the wrong dictionary
// is an error, not silent corruption, so the reader and the writer must be
// carrying the same bytes by construction.
//
//go:embed dict/filmstock.dict
var dictionary []byte

// Dictionary returns the compression dictionary the record stores are built
// with. Exposed so a tool that opens a store directly passes the same one.
func Dictionary() []byte { return dictionary }

// storeOptions are the settings every filmstock record store is opened with.
// They are not configurable: they are recorded in the store header and enforced
// on reopen, so they are a property of the format rather than of a caller.
func storeOptions() []gitdb.Option {
	return []gitdb.Option{
		gitdb.WithEncoding(gitdb.Base94()),
		gitdb.WithDictionary(dictionary),
		// Level 9 costs build time once and is paid back on every clone.
		gitdb.WithLevel(9),
		// Well under the 100 MB a git host refuses, and small enough that a
		// browser still renders the diff.
		gitdb.WithMaxFileSize(32 << 20),
	}
}

// Store serves records from a gitdb record store — one store per kind, under
// root: root/movies, root/television, root/people, root/events.
//
// Kinds get their own stores rather than sharing one so that a diff is scoped to
// what changed, and so a consumer who only wants films is not reading past
// television bytes to find them.
func Store(root string) RecordSource { return &storeSource{root: root, open: map[string]*gitdb.DB{}} }

type storeSource struct {
	root string
	mu   sync.Mutex
	open map[string]*gitdb.DB
}

// db opens a kind's store on first use. Stores are cached because opening one
// reads and validates its header, and a page view should not pay for that.
func (s *storeSource) db(kind string) (*gitdb.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.open[kind]; ok {
		return d, nil
	}
	d, err := gitdb.Open(filepath.Join(s.root, kind), storeOptions()...)
	if err != nil {
		return nil, fmt.Errorf("filmstock: open %s store: %w", kind, err)
	}
	s.open[kind] = d
	return d, nil
}

func (s *storeSource) Fetch(_ context.Context, loc Location) ([]byte, error) {
	if loc.GitdbID == 0 {
		return nil, fmt.Errorf("filmstock: %s %d has no record id; the index was "+
			"built against a different store (rebuild it with `filmstock index`): %w",
			loc.Kind, loc.ID, ErrNotFound)
	}
	d, err := s.db(loc.Kind)
	if err != nil {
		return nil, err
	}
	b, err := d.Get(loc.GitdbID)
	if err != nil {
		return nil, fmt.Errorf("filmstock: %s %d (record %d): %w", loc.Kind, loc.ID, loc.GitdbID, err)
	}
	return b, nil
}

// Close releases every store this source has opened.
func (s *storeSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var first error
	for _, d := range s.open {
		if c, ok := any(d).(interface{ Close() error }); ok {
			if err := c.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

// A StoredRecord is one record as it sits in a store: the store's id for it, and
// its bytes. The identity (page_id, Q-id) is inside Data — the store does not
// know or care what a record means.
type StoredRecord struct {
	GitdbID uint64
	Data    []byte
}

// WalkStore visits every live record of a kind, in store order.
//
// This is how the indexers read the corpus. They need both the record and the
// store's id for it — the id is what the index stores so a reader can get back
// to it — and iterating gives both without anyone having to derive a location.
func WalkStore(root, kind string, fn func(StoredRecord) error) error {
	d, err := gitdb.Open(filepath.Join(root, kind), storeOptions()...)
	if err != nil {
		return fmt.Errorf("filmstock: open %s store: %w", kind, err)
	}
	for rec, err := range d.All() {
		if err != nil {
			return fmt.Errorf("filmstock: walk %s store: %w", kind, err)
		}
		if err := fn(StoredRecord{GitdbID: rec.ID, Data: rec.Data}); err != nil {
			return err
		}
	}
	return nil
}

// OpenStore opens one kind's record store for writing. Callers that only read
// should use Store or WalkStore.
func OpenStore(root, kind string) (*gitdb.DB, error) {
	return gitdb.Open(filepath.Join(root, kind), storeOptions()...)
}
