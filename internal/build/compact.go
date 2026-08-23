package build

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/szatmary/filmstock"
)

// CmdCompact reclaims the space held by superseded and deleted records.
//
// gitdb record files are append-only: Update writes a new version and leaves the
// old bytes where they are, so that no other record's location moves and every
// operation stays a one-line diff. The price is that dead bytes accumulate. A
// full re-extract that changes every record — a schema change, say — leaves the
// store at roughly twice its live size, with the previous version of everything
// still on disk.
//
// Do not run this after every ingest, and the reason is not the one you would
// expect. The record files diff beautifully: compaction rewrites only files
// holding dead bytes and keeps survivors in order, so a .gitdb diff reads as
// removed lines, measured at -174, -1, -1 for a store with a handful of dead
// records.
//
// It is the INDEX shards that are expensive. An entry holds an absolute offset,
// so removing one record shifts every later record in that file and rewrites
// every entry after it. Measured: updating a single record costs 3 changed lines
// (+1 in the record file, +1/-1 in the index); compacting that same store cost
// ~9,900, of which 8,875 were index entries whose offsets moved.
//
// So the trade inverts. Leaving a few dead record lines is far cheaper than the
// index churn of reclaiming them. Compaction pays only when dead bytes are a
// large fraction of the store — after a schema change that rewrote everything,
// where it recovered 783.5 MB -> 420.7 MB, not after a daily update that
// superseded ten records.
func CmdCompact(args []string) {
	fs := flag.NewFlagSet("compact", flag.ExitOnError)
	records := fs.String("records", "filmstock-data", "record store to compact")
	fs.Parse(args)

	start := time.Now()
	var beforeTotal, afterTotal int64
	for _, kind := range []string{
		filmstock.KindMovie, filmstock.KindTelevision,
		filmstock.KindPerson, filmstock.KindEvent,
	} {
		before := dirSize(*records + "/" + kind)
		db, err := filmstock.OpenStore(*records, kind)
		if err != nil {
			fatal(err)
		}
		if err := db.Compact(); err != nil {
			fatal(fmt.Errorf("compacting %s: %w", kind, err))
		}
		after := dirSize(*records + "/" + kind)
		beforeTotal, afterTotal = beforeTotal+before, afterTotal+after
		fmt.Fprintf(os.Stderr, "  %-12s %8.1f MB -> %8.1f MB  %+.1f%%\n",
			kind, float64(before)/(1<<20), float64(after)/(1<<20),
			100*float64(after-before)/float64(before))
	}
	fmt.Fprintf(os.Stderr, "  %-12s %8.1f MB -> %8.1f MB  %+.1f%%   in %.1fs\n",
		"TOTAL", float64(beforeTotal)/(1<<20), float64(afterTotal)/(1<<20),
		100*float64(afterTotal-beforeTotal)/float64(beforeTotal), time.Since(start).Seconds())
	fmt.Fprintln(os.Stderr, "\nids are unchanged, so the index is still valid.")
}
