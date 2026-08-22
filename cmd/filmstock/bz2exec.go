package main

import (
	"bytes"
	"compress/bzip2"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
)

// Decompression of the big dumps is delegated to lbzip2, with Go's stdlib
// decoder as a loud, slow fallback.
//
// bzip2 blocks are independently decodable but BIT-aligned with no length field,
// so parallel decoding means scanning for block magics at arbitrary bit offsets.
// A pure-Go implementation of that was written and verified byte-identical to
// stdlib on real dumps; it reached ~365 MB/s against lbzip2's ~780. Both
// saturated the CPU (1780% vs 1956%), so the gap was entirely per-core decoder
// speed — Go's compress/bzip2 manages ~21 MB/s per core where lbzip2's C decoder
// does ~39. Closing that would mean writing a faster BWT decoder, which is not
// worth it when lbzcat exists, so the Go version was dropped.

// readAheadChunks/Size control the buffer between the decompressor and the
// parser. Without it the consumer's hiccups — a slow record write, a GC pause,
// an fsync — propagate straight back into the decompressor and idle every core.
const (
	readAheadChunks = 64
	readAheadSize   = 4 << 20 // 256 MB of slack in total
)

// readAheadReader keeps a fixed number of chunks buffered ahead of the consumer,
// filled by a goroutine that runs independently of Read calls.
type readAheadReader struct {
	ch   chan []byte
	errC chan error
	cur  []byte
	err  error
}

func newReadAhead(r io.Reader, chunks, size int) *readAheadReader {
	ra := &readAheadReader{ch: make(chan []byte, chunks), errC: make(chan error, 1)}
	go func() {
		defer close(ra.ch)
		for {
			buf := make([]byte, size)
			n, err := io.ReadFull(r, buf)
			if n > 0 {
				ra.ch <- buf[:n]
			}
			if err != nil {
				if err == io.EOF || err == io.ErrUnexpectedEOF {
					ra.errC <- nil
				} else {
					ra.errC <- err
				}
				return
			}
		}
	}()
	return ra
}

func (ra *readAheadReader) Read(p []byte) (int, error) {
	for len(ra.cur) == 0 {
		blk, ok := <-ra.ch
		if !ok {
			if ra.err == nil {
				select {
				case ra.err = <-ra.errC:
				default:
				}
				if ra.err == nil {
					ra.err = io.EOF
				}
			}
			return 0, ra.err
		}
		ra.cur = blk
	}
	n := copy(p, ra.cur)
	ra.cur = ra.cur[n:]
	return n, nil
}

// bz2Cmd is a running lbzcat whose stdout is the decompressed stream.
type bz2Cmd struct {
	cmd    *exec.Cmd
	out    io.ReadCloser
	rd     io.Reader
	stderr bytes.Buffer
}

func (b *bz2Cmd) Read(p []byte) (int, error) {
	n, err := b.rd.Read(p)
	if err == io.EOF {
		if werr := b.cmd.Wait(); werr != nil {
			return n, fmt.Errorf("lbzcat: %v: %s", werr, b.stderr.String())
		}
	}
	return n, err
}

func (b *bz2Cmd) Close() error {
	b.out.Close()
	_ = b.cmd.Process.Kill()
	_, _ = b.cmd.Process.Wait()
	return nil
}

// openBz2 returns a reader over the decompressed contents of a .bz2 file,
// preferring lbzcat and falling back to Go's single-threaded stdlib decoder when
// it is absent. Which path is taken is always reported, never silent.
func openBz2(path string, workers int) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if lb, err := exec.LookPath("lbzcat"); err == nil {
		f.Close()
		cmd := exec.Command(lb, "-n", strconv.Itoa(workers), "-d", "-c", path)
		b := &bz2Cmd{cmd: cmd}
		cmd.Stderr = &b.stderr
		out, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		b.out = out
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		b.rd = newReadAhead(out, readAheadChunks, readAheadSize)
		return b, nil
	}
	fmt.Fprintf(os.Stderr,
		"WARNING: lbzcat not found on PATH — falling back to Go's built-in bzip2\n"+
			"         decoder for %s.\n"+
			"         Output is identical, but it is SINGLE-THREADED: ~72 MB/s versus\n"+
			"         ~780 with lbzip2, so a large dump goes from minutes to hours.\n"+
			"         Install lbzip2.\n", path)
	return &goBz2{f: f, r: newReadAhead(bzip2.NewReader(f), readAheadChunks, readAheadSize)}, nil
}

type goBz2 struct {
	f *os.File
	r io.Reader
}

func (g *goBz2) Read(p []byte) (int, error) { return g.r.Read(p) }
func (g *goBz2) Close() error               { return g.f.Close() }
