package build

import (
	"flag"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/szatmary/filmstock/internal/dump"
)

// Pass 2: the intermediate, into records.
//
// This is extract's second half with the dump taken out from under it. The
// recognisers, the collector and the record writer are the same objects doing
// the same work; only where the pages come from has changed.
//
// That is deliberate rather than lazy. The two-pass split is only worth
// anything if a record built from the intermediate is the record extract would
// have built, and the surest way to make that true is for there to be one
// implementation and two readers. A second copy of the shaping logic would
// drift, and drift here is silent — the records still write, they are just
// quietly different from the ones the expensive path produces.
//
// What it buys: the 24 GB of pages that are not films, people, series or
// schedules never has to be decompressed again. Every question about how a
// record is SHAPED — which fields it carries, how a title displays, whether
// absent means omitted — is answerable against a stable input instead of
// costing a 41-minute re-parse of the whole dump.
func CmdExport(args []string) {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	inter := fs.String("inter", defaultInterPath(), "intermediate store to read")
	dumps := fs.String("dumps", "dump", "directory holding the dumps (for the wikidata cache)")
	outDir := fs.String("out", "records", "record tree to write")
	textDir := fs.String("text", "", "corpus directory (default: alongside the records)")
	workers := fs.Int("workers", 18, "parallel workers")
	skipText := fs.Bool("no-text", false, "skip the plain-text corpus")
	limit := fs.Int("limit", 0, "stop after this many films (0 = all)")
	fs.Parse(args)

	d, err := findDumps(*dumps)
	if err != nil {
		fatal(err)
	}
	in, err := OpenInter(*inter)
	if err != nil {
		fatal(err)
	}
	defer in.Close()

	// Which dump this intermediate came from, stated up front. A stale
	// intermediate exported against a newer wikidata cache produces records that
	// look current and are not, and nothing about the output would say so.
	src, _ := in.Source()
	n, err := in.Pages()
	if err != nil {
		fatal(err)
	}
	if n == 0 {
		fatal(fmt.Errorf("export: %s holds no pages — run `filmstock import` first", *inter))
	}
	fmt.Fprintf(os.Stderr, "export: %d pages from %s\n", n, *inter)
	if src != "" {
		fmt.Fprintf(os.Stderr, "  imported from %s\n", src)
	}

	if *textDir == "" {
		*textDir = *outDir
	}
	start := time.Now()
	if err := extractRecords(d, interSource(in, n), *outDir, *textDir, *workers, !*skipText, *limit); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "export complete in %.1f min\n", time.Since(start).Minutes())
}

// interSource replays the intermediate's pages.
//
// Serial, and unapologetically so: this reads a local SQLite file rather than
// decompressing 25 GB of bzip2, and the parallelism in the dump path exists to
// hide decompression that no longer happens. The handlers it feeds are the same
// ones the dump path feeds, and they are already safe to call concurrently, so
// this can be widened if it ever turns out to matter.
func interSource(in *Inter, total int) pageSource {
	return pageSource{name: in.path, total: int64(total),
		run: func(handle func(dump.Page), stop func() bool, prog *atomic.Int64) error {
			err := in.EachPage(func(p dump.Page) error {
				if stop != nil && stop() {
					return errStopExport
				}
				handle(p)
				if prog != nil {
					prog.Add(1)
				}
				return nil
			})
			if err == errStopExport {
				return nil
			}
			return err
		}}
}

// errStopExport unwinds EachPage when -limit is reached. It is not a failure,
// so it never reaches the caller.
var errStopExport = fmt.Errorf("export: limit reached")
