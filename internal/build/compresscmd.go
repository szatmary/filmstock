package build

import (
	"flag"
	"fmt"
	"os"
	"time"
)

// CmdCompress converts plain SQLite databases into zstdvfs containers in
// place, for a full that was published before compression existed or that was
// deliberately published plain.
//
// The content hash is unaffected — it describes rows, not storage — so a
// build's patches, its chain and its consumers' verification all keep working
// across the conversion. What DOES change is the file's bytes, so the build's
// manifest must be regenerated afterwards:
//
//	filmstock compress -db bucket/20260801/filmstock.db …
//	filmstock manifest -dir bucket/20260801 -dump 20260801
//
// Only safe before anyone has fetched the build: a published URL whose bytes
// change breaks every consumer that cached or half-fetched it.
func CmdCompress(args []string) {
	fs := flag.NewFlagSet("compress", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: filmstock compress FILE.db [FILE.db …]")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() == 0 {
		fatal(fmt.Errorf("compress needs at least one database file"))
	}
	for _, path := range fs.Args() {
		before, err := os.Stat(path)
		if err != nil {
			fatal(err)
		}
		start := time.Now()
		if err := compressDB(path); err != nil {
			fatal(fmt.Errorf("%s: %w", path, err))
		}
		after, err := os.Stat(path)
		if err != nil {
			fatal(err)
		}
		fmt.Fprintf(os.Stderr, "  %-40s %5d MB -> %5d MB (%.2fx) in %.0fs\n",
			path, before.Size()/1048576, after.Size()/1048576,
			float64(before.Size())/float64(after.Size()), time.Since(start).Seconds())
	}
	fmt.Fprintln(os.Stderr, "  regenerate the build's manifest now: filmstock manifest -dir DIR -dump ID")
}
