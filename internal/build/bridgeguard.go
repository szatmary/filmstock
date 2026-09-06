package build

import (
	"database/sql"
	"fmt"
	"sort"

	"github.com/szatmary/filmstock/internal/sqldrv"
)

// The bridge guard: a full may repair anything, but it may only *remove* a
// record whose page is genuinely gone from Wikipedia.
//
// The obvious gate — refuse a bridge over N statements — does not work, and it
// is worth writing down why, because N is tempting every time this is read.
//
// A bridge's size is dominated by legitimate repair. Adds-changes dumps are
// retained about 42 days, so any day the daily job missed for longer than that
// can never be applied; 20260802–20260817 aged out exactly this way, and the
// 20260901 full is what closes that gap. That bridge carries sixteen days of
// edits the chain never saw. No statement count distinguishes "repairing
// sixteen missing days" from "reverting the chain to a stale dump" — they are
// the same magnitude and the same shape. A threshold set high enough to admit
// the first admits the second, and one set low enough to catch the second
// blocks every release that does real work.
//
// So the gate is structural. The failure being guarded against — publishing a
// full whose content predates the tip — manifests as records *disappearing*
// for pages that still exist. That is checkable exactly, against the page set
// of the dump the full was built from, with no constant to tune:
//
//	a record in the tip and not in the new build
//	  is legitimate  iff its page_id is absent from the new dump
//	  is a bug       otherwise
//
// Zero unexplained removals. A deleted article, a merged redirect, a page
// moved out of main namespace all satisfy it; a rewind, a parser regression
// that stops recognising a template, and a truncated import all fail it — and
// those last two are worth catching for their own sake.
//
// Seasons and episodes are deliberately not checked. Their ids are content
// hashes rather than page_ids: most seasons have no article of their own, and
// an episode row vanishing because someone reformatted a "List of episodes"
// table is an ordinary edit, not a deletion.
var bridgeKeyed = []struct{ table, col string }{
	{"movies", "id"},
	{"television_series", "id"},
	{"events", "id"},
	{"people", "page_id"},
}

// removal is one record present in the tip and absent from the new build.
type removal struct {
	table  string
	pageID int64
}

// guardRemovals reports records the bridge would delete whose pages are still
// in the dump the new build was made from.
//
// pageSet is the resolver cache for that dump: its wiki_qid table is rebuilt
// DROP/CREATE from the dump's own multistream index, so it is the dump's page
// set and not a stale copy of an older one.
func guardRemovals(basePath, newPath, pageSet string) error {
	base, err := sql.Open(sqldrv.Name, basePath)
	if err != nil {
		return err
	}
	defer base.Close()
	fresh, err := sql.Open(sqldrv.Name, newPath)
	if err != nil {
		return err
	}
	defer fresh.Close()

	var gone []removal
	for _, k := range bridgeKeyed {
		was, hadTip, err := pageIDs(base, k.table, k.col)
		if err != nil {
			return fmt.Errorf("%s in the tip: %w", k.table, err)
		}
		now, hadNew, err := pageIDs(fresh, k.table, k.col)
		if err != nil {
			return fmt.Errorf("%s in the new build: %w", k.table, err)
		}
		// A table absent from one side is an empty set on that side, which is
		// the honest reading: a table the tip had and the new build does not
		// has removed every record in it, and each one is then checked against
		// the dump like any other. Absent from BOTH is not a fact about the
		// data, it is this list naming a table that does not exist — and a
		// guard that silently checks nothing is worse than no guard.
		if !hadTip && !hadNew {
			return fmt.Errorf("neither build has a %s table; bridgeKeyed names a "+
				"table the schema does not", k.table)
		}
		for id := range was {
			if !now[id] {
				gone = append(gone, removal{k.table, id})
			}
		}
	}
	if len(gone) == 0 {
		return nil
	}

	live, err := stillInDump(pageSet, gone)
	if err != nil {
		return fmt.Errorf("checking removals against %s: %w", pageSet, err)
	}
	if len(live) == 0 {
		return nil
	}

	sort.Slice(live, func(i, j int) bool {
		if live[i].table != live[j].table {
			return live[i].table < live[j].table
		}
		return live[i].pageID < live[j].pageID
	})
	show := live
	if len(show) > 10 {
		show = show[:10]
	}
	msg := fmt.Sprintf("refusing to publish: the bridge removes %d record(s) whose pages\n"+
		"are still present in the dump this build was made from. A full may repair\n"+
		"anything, but a record vanishing for a page that still exists is a rewind,\n"+
		"a parser regression, or a truncated import — never a deletion.\n",
		len(live))
	for _, r := range show {
		msg += fmt.Sprintf("  %-20s page_id %d\n", r.table, r.pageID)
	}
	if len(live) > len(show) {
		msg += fmt.Sprintf("  ... and %d more\n", len(live)-len(show))
	}
	return fmt.Errorf("%s", msg)
}

// pageIDs reads one table's page identities. A zero or NULL key means the
// record has no article of its own, which is not a page and cannot be checked
// against the dump.
func pageIDs(db *sql.DB, table, col string) (map[int64]bool, bool, error) {
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`,
		table).Scan(&n); err != nil {
		return nil, false, err
	}
	if n == 0 {
		return map[int64]bool{}, false, nil
	}
	rows, err := db.Query(fmt.Sprintf(
		"SELECT %s FROM %s WHERE %s IS NOT NULL AND %s != 0", col, table, col, col))
	if err != nil {
		return nil, true, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, true, err
		}
		out[id] = true
	}
	return out, true, rows.Err()
}

// stillInDump returns the removals whose page_id the dump still carries.
func stillInDump(pageSet string, gone []removal) ([]removal, error) {
	db, err := sql.Open(sqldrv.Name, pageSet)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	stmt, err := db.Prepare("SELECT 1 FROM wiki_qid WHERE page_id = ? LIMIT 1")
	if err != nil {
		return nil, fmt.Errorf("%w — is this a resolver cache built by build-qidmap?", err)
	}
	defer stmt.Close()

	var live []removal
	for _, r := range gone {
		var one int
		switch err := stmt.QueryRow(r.pageID).Scan(&one); err {
		case nil:
			live = append(live, r)
		case sql.ErrNoRows: // genuinely gone from Wikipedia
		default:
			return nil, err
		}
	}
	return live, nil
}
