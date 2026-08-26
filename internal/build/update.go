package build

import (
	"flag"
	"fmt"
	"os"
	"time"
)

// CmdUpdate applies one day of Wikimedia adds-changes to the record store.
//
// This is the production daily job, and the only one: catchup runs it once per
// day it is behind rather than batching, so the path that will run every day
// forever is the path that gets exercised.
//
// It is two passes, not one. The day's pages go into the intermediate, and the
// records are re-derived from the whole corpus:
//
//	day -> intermediate      140s, the day's ~66k main-namespace pages
//	intermediate -> records  6 min, every record rebuilt from stored wikitext
//
// The previous implementation applied the day straight to the record store,
// which was far quicker and could not add a person. A day's dump carries the
// pages edited that day; a new film's cast were not edited that day, so their
// articles sat unread in a dump nobody was opening and the credit resolved to
// nothing. 478 such people accumulated over 23 daily updates, none with a
// canonical identity, and none reconcilable without a full re-extract.
//
// Re-deriving everything also gets deletions right for free. A page that stops
// qualifying — an infobox removed, so a film is no longer a film — is dropped
// from the intermediate and simply does not appear in the export.
func CmdUpdate(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	incr := fs.String("incr", "", "adds-changes dump (enwiki-YYYYMMDD-pages-meta-hist-incr.xml.bz2)")
	records := fs.String("records", "filmstock-data", "record store to update in place")
	inter := fs.String("inter", defaultInterPath(), "intermediate store to apply the day to")
	dumps := fs.String("dumps", "dump", "directory holding the dumps (for the wikidata cache)")
	cache := fs.String("cache", "", "resolver db (default <dumps>/resolver.db)")
	textDir := fs.String("text", "", "corpus directory (default: alongside the records)")
	workers := fs.Int("workers", 18, "parallel workers")
	skipText := fs.Bool("no-text", false, "skip the plain-text corpus")
	noWikitext := fs.Bool("no-wikitext", false, "do not store the day's wikitext")
	commit := fs.Bool("commit", false, "commit the change to the record store's git repository")
	message := fs.String("message", "", "commit message (default names the dump applied)")
	fs.Parse(args)

	if *incr == "" {
		fatal(fmt.Errorf("update needs -incr FILE (see dumps.wikimedia.org/other/incr/enwiki/)"))
	}
	d, err := findDumps(*dumps)
	if err != nil {
		fatal(err)
	}
	if *cache != "" {
		d.cache = *cache
	}
	if *textDir == "" {
		*textDir = *records
	}

	start := time.Now()
	fmt.Fprintf(os.Stderr, "[1/2] %s -> %s\n", *incr, *inter)
	importIncr(*incr, *inter, *workers, !*noWikitext)

	fmt.Fprintf(os.Stderr, "\n[2/2] %s -> %s\n", *inter, *records)
	in, err := OpenInter(*inter)
	if err != nil {
		fatal(err)
	}
	defer in.Close()
	if err := runExport(in, d, *records, *textDir, *workers, !*skipText, 0); err != nil {
		fatal(err)
	}

	fmt.Fprintf(os.Stderr, "\napplied %s in %.1f min\n", *incr, time.Since(start).Minutes())
	fmt.Fprintln(os.Stderr, "the index is now stale; rebuild it with `filmstock index`")

	msg := *message
	if msg == "" {
		msg = defaultCommitMessage(*incr)
	}
	if err := reportAndCommit(os.Stderr, *records, msg, *commit); err != nil {
		fatal(err)
	}
}
