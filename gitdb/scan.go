package gitdb

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
)

// scanBufMax bounds one line. A record is capped by the file size anyway, so
// this only has to be large enough not to reject a legal line.
const scanBufMax = 64 << 20

// scan builds the in-memory key map by reading every record file.
//
// This is the work the .idx files used to save, and it is deliberately cheap:
// each line is split at the first two spaces to read the key and version, and
// the payload is never decoded. The last line for a key wins by version, so
// resolution does not depend on file order — which matters after Compact
// reorders lines, and after a merge interleaves two branches' appends.
func (db *DB) scan() error {
	files, err := db.recordFiles()
	if err != nil {
		return err
	}
	db.live = make(map[string]location, 1024)
	db.seen = make(map[string]uint64, 1024)
	db.last = 0
	for _, i := range files {
		if err := db.scanFile(i); err != nil {
			return err
		}
		db.last = i
	}
	return nil
}

func (db *DB) scanFile(i int) error {
	path := db.path(i)
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("gitdb: %w", err)
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 1<<20)
	var offset int64

	header, err := readLine(br)
	if err == io.EOF {
		return nil // empty file
	}
	if err != nil {
		return fmt.Errorf("gitdb: %s: %w", path, err)
	}
	if err := db.checkHeader(path, header); err != nil {
		return err
	}
	offset += int64(len(header)) + 1

	for {
		line, err := readLine(br)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("gitdb: %s: %w", path, err)
		}
		lineStart := offset
		offset += int64(len(line)) + 1
		if len(line) == 0 {
			continue
		}
		key, version, payloadOff, payloadLen, err := splitLine(line)
		if err != nil {
			return fmt.Errorf("gitdb: %s at offset %d: %w", path, lineStart, err)
		}
		// Highest version wins. An equal version is a genuine conflict — two
		// branches wrote the same key independently — and the later file wins,
		// which at least makes the outcome deterministic for a given tree.
		if version > db.seen[key] {
			db.seen[key] = version
		}
		if prev, ok := db.live[key]; ok && prev.version > version {
			continue
		}
		if bytes.Equal(line[payloadOff:payloadOff+payloadLen], []byte(tombstone)) {
			delete(db.live, key)
			continue
		}
		db.live[key] = location{
			file:    i,
			offset:  lineStart + int64(payloadOff),
			length:  payloadLen,
			version: version,
		}
	}
	return nil
}

// splitLine parses "<key> <version> <payload>" without allocating the payload.
func splitLine(line []byte) (key string, version uint64, payloadOff, payloadLen int, err error) {
	a := bytes.IndexByte(line, ' ')
	if a <= 0 {
		return "", 0, 0, 0, fmt.Errorf("line has no key")
	}
	rest := line[a+1:]
	b := bytes.IndexByte(rest, ' ')
	if b <= 0 {
		return "", 0, 0, 0, fmt.Errorf("line has no version")
	}
	version, err = strconv.ParseUint(string(rest[:b]), 10, 64)
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("bad version %q", rest[:b])
	}
	payloadOff = a + 1 + b + 1
	if payloadOff >= len(line) {
		return "", 0, 0, 0, fmt.Errorf("line has no payload")
	}
	return string(line[:a]), version, payloadOff, len(line) - payloadOff, nil
}

// readLine reads one newline-terminated line, returning it without the newline.
// A final line lacking a newline is returned as-is: a store kept in git can be
// checked out that way, and refusing to read it would be worse than accepting it.
func readLine(br *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		chunk, more, err := br.ReadLine()
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		if !more {
			return out, nil
		}
		if len(out) > scanBufMax {
			return nil, fmt.Errorf("line exceeds %d bytes", scanBufMax)
		}
	}
}
