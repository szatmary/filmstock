package gitdb

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"iter"
	"os"
)

// Record is one live record yielded by All.
type Record struct {
	Key     string
	Version uint64
	Data    []byte
}

// All iterates every live record, in file order.
//
// This is the scan a consumer runs to build its own search index, and it is the
// reason the store ships no index of its own: the offset map and the full-text
// content come out of the same pass. Superseded lines and tombstones are skipped
// using the map built at Open, so a compacted and an uncompacted store yield the
// same records.
func (db *DB) All() iter.Seq2[Record, error] {
	return func(yield func(Record, error) bool) {
		files, err := db.recordFiles()
		if err != nil {
			yield(Record{}, err)
			return
		}
		db.mu.RLock()
		live := make(map[string]location, len(db.live))
		for k, v := range db.live {
			live[k] = v
		}
		db.mu.RUnlock()

		for _, i := range files {
			if !db.walkFile(i, live, yield) {
				return
			}
		}
	}
}

func (db *DB) walkFile(i int, live map[string]location, yield func(Record, error) bool) bool {
	path := db.path(i)
	f, err := os.Open(path)
	if err != nil {
		return yield(Record{}, fmt.Errorf("gitdb: %w", err))
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 1<<20)
	var offset int64
	header, err := readLine(br)
	if err == io.EOF {
		return true
	}
	if err != nil {
		return yield(Record{}, fmt.Errorf("gitdb: %s: %w", path, err))
	}
	offset += int64(len(header)) + 1

	for {
		line, err := readLine(br)
		if err == io.EOF {
			return true
		}
		if err != nil {
			return yield(Record{}, fmt.Errorf("gitdb: %s: %w", path, err))
		}
		lineStart := offset
		offset += int64(len(line)) + 1
		if len(line) == 0 {
			continue
		}
		key, version, payloadOff, payloadLen, err := splitLine(line)
		if err != nil {
			return yield(Record{}, fmt.Errorf("gitdb: %s at %d: %w", path, lineStart, err))
		}
		// Only the line the map points at is current; every other line for this
		// key is a superseded version still occupying space.
		loc, ok := live[key]
		if !ok || loc.file != i || loc.offset != lineStart+int64(payloadOff) {
			continue
		}
		if bytes.Equal(line[payloadOff:payloadOff+payloadLen], []byte(tombstone)) {
			continue
		}
		data, err := db.decodeRecord(line[payloadOff : payloadOff+payloadLen])
		if err != nil {
			return yield(Record{}, fmt.Errorf("gitdb: %s %s: %w", path, key, err))
		}
		if !yield(Record{Key: key, Version: version, Data: data}, nil) {
			return false
		}
	}
}
