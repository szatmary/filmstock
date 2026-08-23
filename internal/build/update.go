package build

import (
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
	workers := fs.Int("workers", 4, "bz2 decompression workers")
	cache := fs.String("cache", "wikidata.db", "Wikidata resolver cache; television needs its P179 edges")
	dry := fs.Bool("dry-run", false, "report what would change without writing")
	commit := fs.Bool("commit", false, "commit the change to the record store's git repository")
	message := fs.String("message", "", "commit message (default names the dump applied)")
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

	// Format 5 stores allocated their own record ids, so an update had to load a
	// page_id -> record-id mapping before it could touch anything, and reading it
	// from the index was the fast way to do that. Format 6 keys records by
	// page_id, so there is nothing to load and nothing to be stale against: the
	// -db flag and its fingerprint check are gone with the mapping they guarded.
	// Television needs the Wikidata edges: a season article names its season and
	// never its series, so nothing else can attach it.
	tv, tvErr := newTelevisionUpdater(w, *cache)
	if tvErr != nil {
		fmt.Fprintf(os.Stderr, "television updates disabled: %v\n", tvErr)
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
		if tv != nil {
			tv.consider(p)
		} else if len(parseTelevisionPage(p)) > 0 {
			st.televisionSkipped++
		}
	})
	if err != nil {
		fatal(err)
	}
	if tv != nil {
		tv.apply(*dry)
	}
	if err := w.Err(); err != nil {
		fatal(err)
	}

	what := "applied"
	if *dry {
		what = "would apply"
	}
	// Report what the store actually took, not what was considered. A changed
	// page very often re-derives to identical bytes — the same revision we
	// already extracted — and saying "18 biographies updated" when nine lines
	// changed is the kind of number that gets trusted and then disbelieved.
	unchanged, updated, inserted := w.Counts()
	fmt.Fprintf(os.Stderr, "%s %s in %.1fs\n", what, *incr, time.Since(start).Seconds())
	fmt.Fprintf(os.Stderr, "  pages=%d (ns0=%d)\n", st.pages, st.ns0)
	fmt.Fprintf(os.Stderr, "  store: %d records written (%d updated, %d inserted), %d re-derived identical\n",
		updated+inserted, updated, inserted, unchanged)
	fmt.Fprintf(os.Stderr, "  films        %d changed pages, %d new, %d no longer films\n",
		st.filmUpdated, st.filmAdded, st.filmDropped)
	fmt.Fprintf(os.Stderr, "  events       %d changed pages, %d new, %d no longer events\n",
		st.eventUpdated, st.eventAdded, st.eventDropped)
	fmt.Fprintf(os.Stderr, "  biographies  %d changed pages, %d for people not in the store\n",
		st.bioUpdated, st.bioUnmatched)
	if tv != nil {
		tv.report()
	} else {
		fmt.Fprintf(os.Stderr, "  television   %d pages skipped — no resolver cache\n", st.televisionSkipped)
	}
	if !*dry {
		fmt.Fprintln(os.Stderr, "\nthe index is now stale; rebuild it with `filmstock index`")
		msg := *message
		if msg == "" {
			msg = defaultCommitMessage(*incr)
		}
		if err := reportAndCommit(os.Stderr, *records, msg, *commit); err != nil {
			fatal(err)
		}
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
