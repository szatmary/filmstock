package gitdb

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const compactMarker = ".gitdb-compacting"

// Compact rewrites the store keeping only each key's current version.
//
// Without an on-disk index this is far cheaper in diff terms than it used to be.
// In format 5 every surviving record's byte offset changed, so compaction
// rewrote the whole index: one measured run changed ~9,900 lines, of which 8,875
// were .idx entries whose absolute offsets had shifted. Now only the record files
// move, and the map is rebuilt by scanning, so the diff is the dead lines that
// actually went away.
//
// It is still not something to run daily: every file after the first hole is
// rewritten, so the diff is proportional to the store, not to the change.
func (db *DB) Compact() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	files, err := db.recordFiles()
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}

	marker := filepath.Join(db.dir, compactMarker)
	if err := os.WriteFile(marker, []byte("compacting\n"), 0o666); err != nil {
		return fmt.Errorf("gitdb: %w", err)
	}
	defer os.Remove(marker)

	// Collect the live lines in file order, so a compacted store keeps records
	// near the neighbours they compress well against.
	type liveLine struct {
		key     string
		version uint64
		payload []byte
	}
	var lines []liveLine
	for _, i := range files {
		f, err := os.Open(db.path(i))
		if err != nil {
			return fmt.Errorf("gitdb: %w", err)
		}
		br := bufio.NewReaderSize(f, 1<<20)
		var offset int64
		header, err := readLine(br)
		if err == io.EOF {
			f.Close()
			continue
		}
		if err != nil {
			f.Close()
			return fmt.Errorf("gitdb: %w", err)
		}
		offset += int64(len(header)) + 1
		for {
			line, err := readLine(br)
			if err == io.EOF {
				break
			}
			if err != nil {
				f.Close()
				return fmt.Errorf("gitdb: %w", err)
			}
			lineStart := offset
			offset += int64(len(line)) + 1
			if len(line) == 0 {
				continue
			}
			key, version, po, pl, err := splitLine(line)
			if err != nil {
				f.Close()
				return fmt.Errorf("gitdb: %w", err)
			}
			loc, ok := db.live[key]
			if !ok || loc.file != i || loc.offset != lineStart+int64(po) {
				continue // superseded, or a tombstone
			}
			lines = append(lines, liveLine{
				key:     key,
				version: version,
				payload: append([]byte(nil), line[po:po+pl]...),
			})
		}
		f.Close()
	}

	// Write to temporary files first, then swap. An interrupted compaction
	// leaves the marker and the originals, not a half-rewritten store.
	tmp := make([]string, 0, len(files))
	newLive := make(map[string]location, len(lines))
	fileNo := 1
	var cur *os.File
	var curOff int64
	closeCur := func() error {
		if cur == nil {
			return nil
		}
		err := cur.Close()
		cur = nil
		return err
	}
	defer closeCur()

	for _, l := range lines {
		prefix := fmt.Sprintf("%s %d ", l.key, l.version)
		need := int64(len(prefix) + len(l.payload) + 1)
		if cur != nil && curOff+need > db.maxSize {
			if err := closeCur(); err != nil {
				return fmt.Errorf("gitdb: %w", err)
			}
			fileNo++
		}
		if cur == nil {
			p := db.path(fileNo) + ".tmp"
			f, err := os.Create(p)
			if err != nil {
				return fmt.Errorf("gitdb: %w", err)
			}
			cur, tmp = f, append(tmp, p)
			h := db.header()
			if _, err := cur.WriteString(h); err != nil {
				return fmt.Errorf("gitdb: %w", err)
			}
			curOff = int64(len(h))
		}
		if _, err := cur.WriteString(prefix); err != nil {
			return fmt.Errorf("gitdb: %w", err)
		}
		payloadOff := curOff + int64(len(prefix))
		if _, err := cur.Write(l.payload); err != nil {
			return fmt.Errorf("gitdb: %w", err)
		}
		if _, err := cur.Write([]byte{'\n'}); err != nil {
			return fmt.Errorf("gitdb: %w", err)
		}
		newLive[l.key] = location{
			file:    fileNo,
			offset:  payloadOff,
			length:  len(l.payload),
			version: l.version,
		}
		curOff += need
	}
	if err := closeCur(); err != nil {
		return fmt.Errorf("gitdb: %w", err)
	}

	// Remove the old files, then move the new ones into place.
	for _, i := range files {
		if err := os.Remove(db.path(i)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("gitdb: %w", err)
		}
	}
	for _, p := range tmp {
		if err := os.Rename(p, p[:len(p)-len(".tmp")]); err != nil {
			return fmt.Errorf("gitdb: %w", err)
		}
	}
	db.live = newLive
	// Compaction drops tombstones and superseded lines, so nothing can be
	// resurrected out of them; the surviving versions are the whole history.
	db.seen = make(map[string]uint64, len(newLive))
	for k, loc := range newLive {
		db.seen[k] = loc.version
	}
	db.last = fileNo
	return nil
}
