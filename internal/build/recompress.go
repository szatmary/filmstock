package build

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/szatmary/filmstock"
)

// CmdRecompress rewrites a record store against different dictionaries.
//
// Changing a dictionary invalidates the store built with it: gitdb records the
// dictionary's identity in each file header and refuses to open against a
// different one. So a retrained dictionary means the store must be rewritten,
// and that will be true of every future retraining.
//
// Doing it by re-extracting costs a 43-minute pass over 26.5 GB to re-derive
// records that have not changed. This reads the records straight out of the old
// store and writes them to the new one, which is the same work minus the dump.
//
// Ids are preserved. gitdb allocates from 1 in insertion order, so writing the
// records back in their existing id order reproduces the same ids — which
// matters because the search index maps page_id to those ids, and a consumer
// holding one expects it to keep meaning the same record.
func CmdRecompress(args []string) {
	fs := flag.NewFlagSet("recompress", flag.ExitOnError)
	in := fs.String("in", "filmstock-data", "store to read")
	out := fs.String("out", "", "store to write (must not exist)")
	oldDict := fs.String("old-dict", "", "directory of the dictionaries the input was built with")
	newDict := fs.String("new-dict", "dict", "directory of the dictionaries to write with")
	fs.Parse(args)

	if *out == "" {
		fatal(fmt.Errorf("recompress needs -out"))
	}
	if _, err := os.Stat(*out); err == nil {
		fatal(fmt.Errorf("%s already exists; recompress will not write into an existing store", *out))
	}

	start := time.Now()
	var totalIn, totalOut int64

	// The new store carries the dictionaries it was written with, so it can be
	// read by any build rather than only by this one.
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal(err)
	}
	for _, kind := range []string{
		filmstock.KindMovie, filmstock.KindTelevision,
		filmstock.KindPerson, filmstock.KindEvent,
	} {
		// Prefer the dictionary the input store carries; fall back to a supplied
		// directory only for stores written before dictionaries travelled with
		// their data.
		od, err := filmstock.LoadDictionary(*in, kind)
		if err != nil {
			od, err = readDict(*oldDict, kind, filmstock.Dictionary(kind))
		}
		if err != nil {
			fatal(err)
		}
		nd, err := readDict(*newDict, kind, nil)
		if err != nil {
			fatal(err)
		}
		if err := os.WriteFile(filepath.Join(*out, filmstock.DictionaryName(kind)), nd, 0o644); err != nil {
			fatal(err)
		}

		src, err := filmstock.OpenStoreWithDictionary(*in, kind, od)
		if err != nil {
			fatal(fmt.Errorf("open %s in %s: %w", kind, *in, err))
		}
		if err := os.MkdirAll(filepath.Join(*out, kind), 0o755); err != nil {
			fatal(err)
		}
		dst, err := filmstock.OpenStoreWithDictionary(*out, kind, nd)
		if err != nil {
			fatal(fmt.Errorf("create %s in %s: %w", kind, *out, err))
		}

		// Format 5 allocated ids, so this loop had to reproduce them exactly —
		// including the gaps left by deleted records, which it recreated by
		// inserting a placeholder and deleting it, because a shifted id would
		// have made the index point at the wrong record. Format 6 keys records by
		// their own identity, so a rewrite just writes them.
		var n int
		for rec, err := range src.All() {
			if err != nil {
				fatal(fmt.Errorf("reading %s: %w", kind, err))
			}
			if err := dst.Put(rec.Key, rec.Data); err != nil {
				fatal(fmt.Errorf("writing %s: %w", kind, err))
			}
			n++
		}
		inSz, outSz := dirSize(filepath.Join(*in, kind)), dirSize(filepath.Join(*out, kind))
		totalIn, totalOut = totalIn+inSz, totalOut+outSz
		fmt.Fprintf(os.Stderr, "  %-12s %7d records  %7.1f MB -> %7.1f MB  %+.1f%%\n",
			kind, n, float64(inSz)/(1<<20), float64(outSz)/(1<<20),
			100*float64(outSz-inSz)/float64(inSz))
	}
	fmt.Fprintf(os.Stderr, "  %-12s %7s  %7.1f MB -> %7.1f MB  %+.1f%%   in %.1fs\n",
		"TOTAL", "", float64(totalIn)/(1<<20), float64(totalOut)/(1<<20),
		100*float64(totalOut-totalIn)/float64(totalIn), time.Since(start).Seconds())
}

// readDict loads <dir>/<kind>.dict, falling back to the supplied bytes when no
// directory was given.
func readDict(dir, kind string, embedded []byte) ([]byte, error) {
	if dir == "" {
		if embedded == nil {
			return nil, fmt.Errorf("no dictionary directory given for %s", kind)
		}
		return embedded, nil
	}
	return os.ReadFile(filepath.Join(dir, kind+".dict"))
}

func dirSize(dir string) int64 {
	var n int64
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if fi, err := e.Info(); err == nil {
			n += fi.Size()
		}
	}
	return n
}
