package build

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/szatmary/filmstock"
	"github.com/szatmary/filmstock/internal/dump"
)

// CmdUpdate applies one Wikimedia adds-changes dump to an existing record store.
//
// The full pass reads 26.5 GB and takes 43 minutes because it re-derives the
// whole corpus. A day's changes are ~829 MB compressed and touch about 145 films
// out of 165,265 — under a tenth of a percent — so an incremental pass is the
// difference between rewriting the store and changing a few hundred lines of it.
//
// Two things are deliberately NOT done here, and both are stated rather than
// half-done:
//
// Television is skipped. A season article names its season but never its series,
// and its series may not be in today's changes at all, so attaching it means
// merging into a record the pass never sees. That is real work and it belongs in
// its own change.
//
// Deletions of pages are not detected. An adds-changes dump carries pages that
// changed; a page deleted from Wikipedia simply stops appearing, which is
// indistinguishable from a page that did not change. Only a full pass, or a
// separate page list, can find those. What IS handled is a page that stops
// QUALIFYING — an infobox removed, so a film is no longer a film — because that
// page does appear, and leaving the old record would be a silent lie.
func CmdUpdate(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	incr := fs.String("incr", "", "adds-changes dump (enwiki-YYYYMMDD-pages-meta-hist-incr.xml.bz2)")
	records := fs.String("records", "filmstock-data", "record store to update in place")
	dbPath := fs.String("db", "", "optional index; read for its page_id -> record mapping instead of scanning the store")
	workers := fs.Int("workers", 4, "bz2 decompression workers")
	dry := fs.Bool("dry-run", false, "report what would change without writing")
	fs.Parse(args)

	if *incr == "" {
		fatal(fmt.Errorf("update needs -incr FILE (see dumps.wikimedia.org/other/incr/enwiki/)"))
	}
	rc, err := dump.OpenBz2(*incr, *workers)
	if err != nil {
		fatal(err)
	}
	defer rc.Close()

	w := newStoreWriter(*records)

	// The index is an optimisation, not a dependency. It already holds
	// page_id -> gitdb_id, so reading it turns a 23-second scan of the whole
	// store into four queries. But this is an offline maintainer's tool, and a
	// maintainer with a store and no index must be able to apply a day's changes
	// and index afterwards — the same order an end user does it in.
	//
	// When an index IS given its fingerprint is checked first: applying changes
	// through a mapping built from a different state of the store would update
	// the wrong records, which is worse than being slow.
	if *dbPath != "" {
		if err := filmstock.CheckIndexAgainstStore(*dbPath, *records); err != nil {
			fatal(err)
		}
		idx, err := sql.Open("sqlite", *dbPath)
		if err != nil {
			fatal(err)
		}
		defer idx.Close()
		if err := w.loadIdentitiesFromIndex(idx); err != nil {
			fatal(err)
		}
		fmt.Fprintf(os.Stderr, "identities from %s\n", *dbPath)
	} else {
		fmt.Fprintln(os.Stderr, "no -db given; reading identities by scanning the store (slower)")
	}
	start := time.Now()
	var st updateStats

	err = dump.RunStream(rc, func(p dump.Page) {
		st.pages++
		if p.NS != 0 {
			return
		}
		st.ns0++
		applyFilm(w, p, &st, *dry)
		applyEvent(w, p, &st, *dry)
		applyBiography(w, p, &st, *dry)
		if len(parseTelevisionPage(p)) > 0 {
			st.televisionSkipped++
		}
	})
	if err != nil {
		fatal(err)
	}
	if err := w.Err(); err != nil {
		fatal(err)
	}

	what := "applied"
	if *dry {
		what = "would apply"
	}
	fmt.Fprintf(os.Stderr, "%s %s in %.1fs\n", what, *incr, time.Since(start).Seconds())
	fmt.Fprintf(os.Stderr, "  pages=%d (ns0=%d)\n", st.pages, st.ns0)
	fmt.Fprintf(os.Stderr, "  films        %d updated, %d added, %d no longer films\n",
		st.filmUpdated, st.filmAdded, st.filmDropped)
	fmt.Fprintf(os.Stderr, "  events       %d updated, %d added, %d no longer events\n",
		st.eventUpdated, st.eventAdded, st.eventDropped)
	fmt.Fprintf(os.Stderr, "  biographies  %d updated, %d for people not in the store\n",
		st.bioUpdated, st.bioUnmatched)
	fmt.Fprintf(os.Stderr, "  television   %d pages skipped — needs cross-page merge, not implemented\n",
		st.televisionSkipped)
	if !*dry {
		fmt.Fprintln(os.Stderr, "\nthe index is now stale; rebuild it with `filmstock index`")
	}
}

type updateStats struct {
	pages, ns0                             int
	filmUpdated, filmAdded, filmDropped    int
	eventUpdated, eventAdded, eventDropped int
	bioUpdated, bioUnmatched               int
	televisionSkipped                      int
}

func applyFilm(w *storeWriter, p dump.Page, st *updateStats, dry bool) {
	m := buildFilm(p)
	had := w.get(filmstock.KindMovie, int64(p.ID)) != nil
	switch {
	case m != nil && had:
		st.filmUpdated++
	case m != nil:
		st.filmAdded++
	case had:
		// The page is still here but is no longer a film. Leaving the record
		// would keep answering queries with something Wikipedia no longer says.
		st.filmDropped++
		if !dry {
			w.delete(filmstock.KindMovie, int64(p.ID))
		}
		return
	default:
		return
	}
	if !dry {
		w.put(filmstock.KindMovie, int64(p.ID), m)
	}
}

func applyEvent(w *storeWriter, p dump.Page, st *updateStats, dry bool) {
	e := buildEvent(p)
	had := w.get(filmstock.KindEvent, int64(p.ID)) != nil
	switch {
	case e != nil && had:
		st.eventUpdated++
	case e != nil:
		st.eventAdded++
	case had:
		st.eventDropped++
		if !dry {
			w.delete(filmstock.KindEvent, int64(p.ID))
		}
		return
	default:
		return
	}
	if !dry {
		w.put(filmstock.KindEvent, int64(p.ID), e)
	}
}

// applyBiography merges a changed article into the person it describes.
//
// It never CREATES a person. A person exists because something credited them;
// a biography arriving for someone nobody has credited is not evidence of a
// person this database should hold, and inventing one would key a record on an
// article title rather than on a credit.
func applyBiography(w *storeWriter, p dump.Page, st *updateStats, dry bool) {
	b := buildBiography(p)
	if b == nil {
		return
	}
	identity, ok := w.personIdentityFor(p.Title)
	if !ok {
		st.bioUnmatched++
		return
	}
	cur := w.get(filmstock.KindPerson, identity)
	if cur == nil {
		st.bioUnmatched++
		return
	}
	var rec filmstock.PersonRecord
	if json.Unmarshal(cur, &rec) != nil {
		return
	}
	rec.PersonBio = b
	st.bioUpdated++
	if !dry {
		w.put(filmstock.KindPerson, identity, rec)
	}
}
