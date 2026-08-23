package dump

import (
	"bufio"
	"compress/bzip2"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// streamRange is a byte range within the multistream .bz2 file spanning one or
// more independent bzip2 sub-streams (each ~100 pages).
type streamRange struct{ start, end int64 }

// loadOffsets reads the multistream index and returns the sorted, unique byte
// offsets that begin each bzip2 sub-stream. Index lines are "offset:pageid:title".
func loadOffsets(indexPath string) ([]int64, error) {
	f, err := os.Open(indexPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = bufio.NewReaderSize(f, 1<<20)
	if strings.HasSuffix(indexPath, ".bz2") {
		r = bzip2.NewReader(r)
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	set := make(map[int64]struct{})
	for sc.Scan() {
		line := sc.Text()
		c := strings.IndexByte(line, ':')
		if c < 0 {
			continue
		}
		off, err := strconv.ParseInt(line[:c], 10, 64)
		if err != nil {
			continue
		}
		set[off] = struct{}{}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	offsets := make([]int64, 0, len(set))
	for off := range set {
		offsets = append(offsets, off)
	}
	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
	return offsets, nil
}

// runMultistream decodes the dump in parallel by assigning independent sub-stream
// byte ranges to workers. Each worker seeks into its own file handle, so there is
// no contention on decoding. Offsets beyond the current file size are skipped,
// which lets this run against a partially-downloaded dump.
// Progress reports how far through the dump the run is, in BYTES.
//
// Bytes, not pages: the total page count is only known once a pass ends and
// changes with every dump, while the dump size and the offset index are both
// known before it starts. Bytes also predict time honestly here, because the job
// is I/O bound — the pages/s figure swings 45x with article size, reading 174/s
// early and 7,891/s later, which is not the job speeding up.
type Progress struct {
	Done, Total int64
}

func RunMultistream(dumpPath, indexPath string, workers int, handle func(Page), shouldStop func() bool) error {
	return RunMultistreamProgress(dumpPath, indexPath, workers, handle, shouldStop, nil)
}

// RunMultistreamProgress is RunMultistream with a progress counter the caller
// may read at any time from another goroutine.
func RunMultistreamProgress(dumpPath, indexPath string, workers int, handle func(Page), shouldStop func() bool, prog *atomic.Int64) error {
	offsets, err := loadOffsets(indexPath)
	if err != nil {
		return fmt.Errorf("loading index: %w", err)
	}
	fi, err := os.Stat(dumpPath)
	if err != nil {
		return err
	}
	size := fi.Size()
	fmt.Fprintf(os.Stderr, "multistream: %d sub-streams, dump size %d bytes\n", len(offsets), size)
	if prog != nil {
		prog.Store(0)
	}

	// Batch consecutive sub-streams to amortise per-job overhead.
	const batch = 20
	jobs := make(chan streamRange, workers*4)
	go func() {
		defer close(jobs)
		for i := 0; i < len(offsets); i += batch {
			start := offsets[i]
			if start >= size {
				break // not downloaded yet
			}
			end := size
			if j := i + batch; j < len(offsets) {
				if offsets[j] <= size {
					end = offsets[j]
				}
			}
			jobs <- streamRange{start, end}
			if shouldStop != nil && shouldStop() {
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, err := os.Open(dumpPath)
			if err != nil {
				fmt.Fprintln(os.Stderr, "open:", err)
				return
			}
			defer f.Close()
			for r := range jobs {
				processRange(f, r, handle)
				if prog != nil {
					prog.Add(r.end - r.start)
				}
				if shouldStop != nil && shouldStop() {
					return
				}
			}
		}()
	}
	wg.Wait()
	return nil
}

// processRange decompresses one byte range and feeds its <page> elements to
// handle. The decompressed block is a bare sequence of <page>…</page> siblings
// (no single root), which xml.Decoder tokenises fine.
func processRange(f *os.File, r streamRange, handle func(Page)) {
	sr := io.NewSectionReader(f, r.start, r.end-r.start)
	bz := bzip2.NewReader(bufio.NewReaderSize(sr, 1<<20))
	dec := xml.NewDecoder(bz)
	for {
		tok, err := dec.Token()
		if err != nil {
			return // EOF or end of this range's streams
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "page" {
			continue
		}
		var p Page
		if err := dec.DecodeElement(&p, &se); err != nil {
			return
		}
		handle(p)
	}
}
