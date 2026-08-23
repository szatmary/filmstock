// Package gitdb stores records as lines of text in append-only files, sized to
// live in a git repository.
//
// # Why there is no index on disk
//
// Earlier versions kept .idx files mapping a record id to its byte offset. They
// were removed, because a consumer has to scan every record anyway to build its
// own search index, and that same scan can build the offset map for free. The
// index was not saving the client any work; it was only adding cost to the
// repository. On a real 450k-record store the .idx files rewrote ~540 lines and
// ~4.8 MB of blob on a day when the data itself appended ~540 lines — roughly
// 38% of daily growth, scaling with the size of the store rather than with the
// size of the change.
//
// So a store is just record files. The key and version are a plaintext prefix on
// each line, which is what makes the scan cheap: building the map reads and
// splits lines without base94-decoding or inflating anything, measured at ~0.12
// microseconds per record against ~123 microseconds to fully decode one.
//
// # Format
//
//	gitdb 6 base94 9 a8848817878b6b0f      <- header line, one per file
//	<key> <version> <encoded payload>      <- one record per line
//	<key> <version> -                      <- a deletion
//
// Files are append-only: updating a record appends a new line with a higher
// version, and the previous line stays where it is. Nothing already written ever
// moves, so a day's changes are a pure append in the diff. Compact reclaims the
// superseded lines when you choose to pay for the larger diff.
package gitdb

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
)

const (
	fileExt       = ".gitdb"
	headerMagic   = "gitdb"
	formatVersion = "6"
	noDictID      = "-"
	defaultLevel  = 6

	// GitHub warns at 50 MB and blocks pushes at 100 MB.
	defaultMaxSize = 50 << 20

	// maxFileSizeLimit is the largest cap gitdb accepts, set below the 100 MB
	// file a git host will refuse, so the format cannot produce one.
	maxFileSizeLimit = 99_999_999

	// tombstone marks a deleted key. A record payload is base94, which never
	// produces a bare "-", so this cannot collide with real data.
	tombstone = "-"
)

// ErrNotFound is returned for a key that is absent or deleted.
var ErrNotFound = errors.New("gitdb: record not found")

// DB is a directory of append-only record files.
//
// Safe for concurrent use within one process. It does not coordinate with other
// processes.
type DB struct {
	dir     string
	enc     Encoding
	level   int
	dict    []byte
	dictID  string
	maxSize int64

	mu   sync.RWMutex
	live map[string]location // key -> where its current version lives
	// seen records the highest version ever written for a key, INCLUDING keys
	// that are currently deleted. Without it a resurrected key restarts at
	// version 1, which is lower than the tombstone that deleted it, and a scan
	// resolving by highest version would resurrect the tombstone instead of the
	// record. Deletions are rare enough that this map stays small relative to
	// live, but it must outlive them.
	seen map[string]uint64
	last int // highest-numbered record file
}

// location is where a key's current line sits, and the version it carries.
type location struct {
	file    int
	offset  int64
	length  int
	version uint64
}

// Option configures a DB at Open time.
type Option func(*DB)

// WithEncoding selects the line encoding for new files. Default: Base94.
func WithEncoding(e Encoding) Option { return func(db *DB) { db.enc = e } }

// WithLevel sets the zlib compression level (0-9). Default: 6.
func WithLevel(level int) Option { return func(db *DB) { db.level = level } }

// WithDictionary sets a zlib preset dictionary shared by every record.
func WithDictionary(dict []byte) Option { return func(db *DB) { db.dict = dict } }

// WithMaxFileSize caps the size of every file the db writes. Default: 50 MB.
//
// Unlike format 5 this is a pure file-size cap: it no longer determines how an
// id maps to an index entry, so changing it for an existing store is harmless.
// Existing files keep their size; only new ones honour the new cap.
func WithMaxFileSize(n int64) Option { return func(db *DB) { db.maxSize = n } }

// Open opens (creating if necessary) the directory and scans every record file
// to build the in-memory key map.
//
// The scan is the cost that replaces the on-disk index. It reads each line's key
// and version prefix and records the offset, without decoding payloads.
func Open(dir string, opts ...Option) (*DB, error) {
	db := &DB{
		dir:     dir,
		enc:     Base94(),
		level:   defaultLevel,
		maxSize: defaultMaxSize,
		live:    map[string]location{},
		seen:    map[string]uint64{},
	}
	for _, opt := range opts {
		opt(db)
	}
	if db.level < 0 || db.level > 9 {
		return nil, fmt.Errorf("gitdb: compression level %d out of range 0-9", db.level)
	}
	if db.maxSize > maxFileSizeLimit {
		return nil, fmt.Errorf("gitdb: max file size %d exceeds the %d-byte limit, which is "+
			"set below the 100 MB file a git host will refuse", db.maxSize, int64(maxFileSizeLimit))
	}
	db.dictID = noDictID
	if len(db.dict) > 0 {
		sum := sha256.Sum256(db.dict)
		db.dictID = hex.EncodeToString(sum[:8])
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return nil, fmt.Errorf("gitdb: %w", err)
	}
	if _, err := os.Stat(filepath.Join(dir, compactMarker)); err == nil {
		return nil, fmt.Errorf("gitdb: %s: a compaction was interrupted and files may be "+
			"half-rewritten; restore the directory (for a db kept in git, \"git checkout\" it) "+
			"and remove %s", dir, compactMarker)
	}
	if err := db.scan(); err != nil {
		return nil, err
	}
	return db, nil
}

// Len reports how many live keys the store holds.
func (db *DB) Len() int {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return len(db.live)
}

func (db *DB) header() string {
	return fmt.Sprintf("%s %s %s %d %s\n", headerMagic, formatVersion, db.enc.Name(), db.level, db.dictID)
}

// checkHeader validates a file's header against this DB's configuration. Reading
// a store written with a different dictionary would inflate to garbage, so this
// is a hard error rather than a warning.
func (db *DB) checkHeader(name string, line []byte) error {
	f := strings.Fields(string(line))
	if len(f) != 5 || f[0] != headerMagic {
		return fmt.Errorf("gitdb: %s: not a gitdb file", name)
	}
	if f[1] != formatVersion {
		return fmt.Errorf("gitdb: %s: format version %s, want %s", name, f[1], formatVersion)
	}
	if f[2] != db.enc.Name() {
		return fmt.Errorf("gitdb: %s: encoded with %s, opened with %s", name, f[2], db.enc.Name())
	}
	if f[4] != db.dictID {
		return fmt.Errorf("gitdb: %s: written with dictionary %s, opened with %s; "+
			"records would inflate to garbage", name, f[4], db.dictID)
	}
	return nil
}

// recordFiles lists the record file numbers present, in order.
func (db *DB) recordFiles() ([]int, error) {
	entries, err := os.ReadDir(db.dir)
	if err != nil {
		return nil, fmt.Errorf("gitdb: %w", err)
	}
	var out []int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, fileExt) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(name, fileExt))
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	slices.Sort(out)
	return out, nil
}

func (db *DB) path(i int) string {
	return filepath.Join(db.dir, fmt.Sprintf("%06d%s", i, fileExt))
}

func (db *DB) encodeRecord(rec []byte) ([]byte, error) {
	if i := bytes.IndexByte(rec, '\n'); i >= 0 {
		return nil, fmt.Errorf("gitdb: record contains a newline at byte %d", i)
	}
	var buf bytes.Buffer
	var zw *zlib.Writer
	var err error
	if len(db.dict) > 0 {
		zw, err = zlib.NewWriterLevelDict(&buf, db.level, db.dict)
	} else {
		zw, err = zlib.NewWriterLevel(&buf, db.level)
	}
	if err != nil {
		return nil, fmt.Errorf("gitdb: %w", err)
	}
	if _, err := zw.Write(rec); err != nil {
		return nil, fmt.Errorf("gitdb: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("gitdb: %w", err)
	}
	return db.enc.Encode(buf.Bytes()), nil
}

func (db *DB) decodeRecord(payload []byte) ([]byte, error) {
	raw, err := db.enc.Decode(payload)
	if err != nil {
		return nil, fmt.Errorf("gitdb: %w", err)
	}
	var zr io.ReadCloser
	if len(db.dict) > 0 {
		zr, err = zlib.NewReaderDict(bytes.NewReader(raw), db.dict)
	} else {
		zr, err = zlib.NewReader(bytes.NewReader(raw))
	}
	if err != nil {
		return nil, fmt.Errorf("gitdb: %w", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("gitdb: %w", err)
	}
	return out, nil
}

// validKey rejects anything that would break the line format. A key with a space
// in it would be silently truncated by the scanner, so it must be refused at the
// point of writing rather than discovered as a missing record later.
func validKey(key string) error {
	if key == "" {
		return errors.New("gitdb: empty key")
	}
	if strings.ContainsAny(key, " \n\t\r") {
		return fmt.Errorf("gitdb: key %q contains whitespace", key)
	}
	return nil
}

// Put stores rec under key, superseding any previous version.
func (db *DB) Put(key string, rec []byte) error {
	if err := validKey(key); err != nil {
		return err
	}
	payload, err := db.encodeRecord(rec)
	if err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.appendLocked(key, payload)
}

// PutString is Put for a string record.
func (db *DB) PutString(key, rec string) error { return db.Put(key, []byte(rec)) }

// Delete removes key, appending a tombstone. The record's bytes stay in place
// until Compact.
func (db *DB) Delete(key string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, ok := db.live[key]; !ok {
		return ErrNotFound
	}
	return db.appendLocked(key, []byte(tombstone))
}

// appendLocked writes one line and updates the map. Caller holds db.mu.
func (db *DB) appendLocked(key string, payload []byte) error {
	version := db.seen[key] + 1
	prefix := key + " " + strconv.FormatUint(version, 10) + " "
	line := make([]byte, 0, len(prefix)+len(payload)+1)
	line = append(line, prefix...)
	line = append(line, payload...)
	line = append(line, '\n')

	if int64(len(line)) > db.maxSize {
		return fmt.Errorf("gitdb: record for %q is %d bytes, over the %d-byte file cap",
			key, len(line), db.maxSize)
	}
	i := db.last
	if i == 0 {
		i = 1
	}
	off, err := db.fileEnd(i)
	if err != nil {
		return err
	}
	// Roll to a new file rather than exceed the cap.
	if off > 0 && off+int64(len(line)) > db.maxSize {
		i++
		off = 0
	}
	f, err := os.OpenFile(db.path(i), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o666)
	if err != nil {
		return fmt.Errorf("gitdb: %w", err)
	}
	defer f.Close()
	if off == 0 {
		h := db.header()
		if _, err := f.WriteString(h); err != nil {
			return fmt.Errorf("gitdb: %w", err)
		}
		off = int64(len(h))
	}
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("gitdb: %w", err)
	}
	db.last = i
	db.seen[key] = version
	if bytes.Equal(payload, []byte(tombstone)) {
		delete(db.live, key)
		return nil
	}
	db.live[key] = location{
		file:    i,
		offset:  off + int64(len(prefix)),
		length:  len(payload),
		version: version,
	}
	return nil
}

// fileEnd returns the current size of record file i, or 0 if absent.
func (db *DB) fileEnd(i int) (int64, error) {
	fi, err := os.Stat(db.path(i))
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("gitdb: %w", err)
	}
	return fi.Size(), nil
}

// Get returns the current record for key.
func (db *DB) Get(key string) ([]byte, error) {
	db.mu.RLock()
	loc, ok := db.live[key]
	db.mu.RUnlock()
	if !ok {
		return nil, ErrNotFound
	}
	payload := make([]byte, loc.length)
	f, err := os.Open(db.path(loc.file))
	if err != nil {
		return nil, fmt.Errorf("gitdb: %w", err)
	}
	defer f.Close()
	if _, err := f.ReadAt(payload, loc.offset); err != nil {
		return nil, fmt.Errorf("gitdb: %s at %d: %w", key, loc.offset, err)
	}
	return db.decodeRecord(payload)
}

// Has reports whether key is present without decoding it.
func (db *DB) Has(key string) bool {
	db.mu.RLock()
	defer db.mu.RUnlock()
	_, ok := db.live[key]
	return ok
}

// Version returns the current version of key, and whether it is present. A
// consumer that has already indexed a key at some version can use this to skip
// re-reading it.
func (db *DB) Version(key string) (uint64, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	loc, ok := db.live[key]
	return loc.version, ok
}

// Keys returns every live key, unordered.
func (db *DB) Keys() []string {
	db.mu.RLock()
	defer db.mu.RUnlock()
	out := make([]string, 0, len(db.live))
	for k := range db.live {
		out = append(out, k)
	}
	return out
}
