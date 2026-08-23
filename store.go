package filmstock

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
// There is one per kind rather than one shared. Each kind is its own store, so
// this costs nothing structurally, and it is worth 15.8% on people and 27.4% on
// events over a shared dictionary — the small, repetitive kinds where a record
// compressed on its own has least to work with. Films and series gain only ~1.5%
// because they are large enough to compress well unaided.
//
// They are embedded rather than read from disk because a store records its
// dictionary's identity in its header: opening a store with the wrong dictionary
// is an error, not silent corruption, so reader and writer must be carrying the
// same bytes by construction.
//
// CAVEAT, and it is a real one: the people dictionary was trained on records
// averaging 36 bytes, because biographies are joined at the END of an extract
// pass and the bounded run used for training never reached most of them. Real
// people records carry a biography 54% of the time and average an order of
// magnitude larger. Retrain against a full store — `filmstock train-dict` — and
// rebuild before believing any number about people compression.
//
//go:embed dict/movies.dict
var dictMovies []byte

//go:embed dict/television.dict
var dictTelevision []byte

//go:embed dict/people.dict
var dictPeople []byte

//go:embed dict/events.dict
var dictEvents []byte

// Dictionary returns the compression dictionary a kind's store is built with.
// Exposed so a tool opening a store directly passes the same one.
func Dictionary(kind string) []byte {
	switch kind {
	case KindMovie:
		return dictMovies
	case KindTelevision:
		return dictTelevision
	case KindPerson:
		return dictPeople
	case KindEvent:
		return dictEvents
	}
	return nil
}

// storeOptions are the settings every filmstock record store is opened with.
// They are not configurable: they are recorded in the store header and enforced
// on reopen, so they are a property of the format rather than of a caller.
func storeOptions(kind string) []gitdb.Option {
	return storeOptionsWith(Dictionary(kind))
}

// storeOptionsWith is the same settings against a supplied dictionary. These are
// not configurable: they are recorded in the store header and enforced on
// reopen, so they are a property of the format rather than of a caller.
func storeOptionsWith(dict []byte) []gitdb.Option {
	return []gitdb.Option{
		gitdb.WithEncoding(gitdb.Base94()),
		gitdb.WithDictionary(dict),
		// Level 9 costs build time once and is paid back on every clone.
		gitdb.WithLevel(9),
		// 4 MB, not the 50 MB default. A record is read by offset and length, so
		// file size does not affect a local lookup at all — but it decides how
		// much has to move for anything that fetches whole files, how much a
		// compaction rewrites, and whether a web diff viewer will render the
		// file at all. Small files cost nothing in capacity here: gitdb spends
		// an index entry's bits on whatever the cap does not need, so a smaller
		// cap buys back file-index bits and total capacity stays in terabytes.
		//
		// It also caps a single record, since gitdb refuses any record larger
		// than the file it would live in. The largest record in this corpus is a
		// 490 KB television series before compression, so 4 MB leaves about an
		// order of magnitude of headroom for a longer-running show.
		gitdb.WithMaxFileSize(4 << 20),
	}
}

// DictionaryName is the file a store keeps its compression dictionary in, one
// per kind, beside the stores themselves.
//
// The dictionary lives with the DATA, not with the code. It is trained on the
// corpus, it changes when the corpus changes, and a store cannot be read without
// exactly the one it was written with — gitdb records only the dictionary's
// identity in each file header, not its bytes. Keeping it in the code repository
// would mean a store could only be opened by the matching version of the tools,
// and every retraining would silently strand older code.
func DictionaryName(kind string) string { return kind + ".dict" }

// LoadDictionary reads the dictionary a store was built with.
//
// A missing dictionary is an error rather than a quiet reach for whatever this
// binary happens to embed: substituting the wrong dictionary does not fail
// cleanly, it surfaces later as unreadable records.
func LoadDictionary(root, kind string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(root, DictionaryName(kind)))
	if err != nil {
		return nil, fmt.Errorf("filmstock: %s has no %s: a store carries the "+
			"dictionary it was written with, and cannot be read without it: %w",
			root, DictionaryName(kind), err)
	}
	return b, nil
}

// WriteDictionaries seeds a new store directory with the dictionaries this build
// embeds. Called when a store is created; the copy on disk is authoritative
// from then on.
func WriteDictionaries(root string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	for _, kind := range []string{KindMovie, KindTelevision, KindPerson, KindEvent} {
		p := filepath.Join(root, DictionaryName(kind))
		if _, err := os.Stat(p); err == nil {
			continue // already seeded; never overwrite what a store was built with
		}
		if err := os.WriteFile(p, Dictionary(kind), 0o644); err != nil {
			return err
		}
	}
	return nil
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
	dict, err := LoadDictionary(s.root, kind)
	if err != nil {
		return nil, err
	}
	d, err := gitdb.Open(filepath.Join(s.root, kind), storeOptionsWith(dict)...)
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
	dict, err := LoadDictionary(root, kind)
	if err != nil {
		return err
	}
	d, err := gitdb.Open(filepath.Join(root, kind), storeOptionsWith(dict)...)
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
	dict, err := LoadDictionary(root, kind)
	if err != nil {
		return nil, err
	}
	return gitdb.Open(filepath.Join(root, kind), storeOptionsWith(dict)...)
}

// OpenStoreWithDictionary opens a store against a dictionary other than the one
// embedded in this build.
//
// It exists for exactly one job: rewriting a store when the dictionary changes.
// A store records its dictionary's identity in its header and refuses to open
// against a different one, so reading the old store and writing the new one has
// to happen in a process holding both — which no single embedded dictionary can
// do. Nothing else should need this.
func OpenStoreWithDictionary(root, kind string, dict []byte) (*gitdb.DB, error) {
	return gitdb.Open(filepath.Join(root, kind), storeOptionsWith(dict)...)
}

// StoreFingerprint identifies the state of a record store.
//
// It hashes the index shards rather than the record files. A shard holds one
// fixed-width entry per id, and every insert, update and delete rewrites the
// entry for the id it touched — so the shards change if and only if the records
// do, and they are small enough (a few MB across the whole corpus) to hash in
// milliseconds at startup.
//
// This exists because "clone and index" has one failure mode: `git pull` brings
// new records and leaves the index behind, and a stale index is not obviously
// wrong. It answers correct-looking queries against yesterday's corpus.
func StoreFingerprint(root string) (string, error) {
	h := sha256.New()
	for _, kind := range []string{KindMovie, KindTelevision, KindPerson, KindEvent} {
		shards, err := filepath.Glob(filepath.Join(root, kind, "*.idx"))
		if err != nil {
			return "", err
		}
		sort.Strings(shards)
		for _, s := range shards {
			f, err := os.Open(s)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(h, "%s\n", filepath.Base(s))
			if _, err := io.Copy(h, f); err != nil {
				f.Close()
				return "", err
			}
			f.Close()
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ErrStaleIndex means the index was built from a different state of the record
// store than the one on disk — almost always a `git pull` without a reindex.
var ErrStaleIndex = errors.New("filmstock: index is stale; rebuild it with `filmstock index`")

// CheckStore reports whether this index was built from the store at root.
//
// It is not called automatically by Open, because a caller may deliberately want
// to search an older index, and because failing to open is a harsh answer to a
// question the caller might rather warn about. Callers that care should call it
// and decide.
func (db *DB) CheckStore(root string) error {
	var want string
	err := db.sql.QueryRow(`SELECT value FROM meta WHERE key = 'store_fingerprint'`).Scan(&want)
	if err == sql.ErrNoRows || err != nil && strings.Contains(err.Error(), "no such table") {
		// An index built before fingerprints existed. Say nothing rather than
		// claim staleness we cannot actually establish.
		return nil
	}
	if err != nil {
		return err
	}
	got, err := StoreFingerprint(root)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("index built from store %s, on disk is %s: %w", want[:12], got[:12], ErrStaleIndex)
	}
	return nil
}

// CheckIndexAgainstStore opens an index and verifies it was built from the store
// at root. Used by tools that need the index's page_id -> record mapping to be
// trustworthy before they write anything: applying a day's changes through a
// stale mapping would update the wrong records.
func CheckIndexAgainstStore(indexPath, root string) error {
	db, err := Open(indexPath, nil)
	if err != nil {
		return err
	}
	defer db.Close()
	return db.CheckStore(root)
}
