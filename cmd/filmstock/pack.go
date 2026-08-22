package main

import (
	"bufio"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/szatmary/filmstock"
)

// cmdPack concatenates every record of a kind into one <kind>.pack and writes
// each record's (offset, length) back into the database.
//
// One pack per kind, not one for everything: a single pack would sit close to
// GitHub's 2 GB per-asset limit and grow with every ingest, and a consumer who
// only wants films should never pay for the television bytes.
//
// Records are appended in ascending id order, so the pack is a pure function of
// its inputs — the same records always produce the same bytes. That is what
// makes a pack diffable between ingests, which is the point of §D1.
func cmdPack(args []string) {
	fs := flag.NewFlagSet("pack", flag.ExitOnError)
	records := fs.String("records", "out", "record hierarchy produced by `extract`")
	dbPath := fs.String("db", "", "database to write offsets into (default <records>/search.db)")
	outDir := fs.String("out", "", "directory for the .pack files (default <records>/packs)")
	fs.Parse(args)

	dbFile := *dbPath
	if dbFile == "" {
		dbFile = filepath.Join(*records, "search.db")
	}
	packDir := *outDir
	if packDir == "" {
		packDir = filepath.Join(*records, "packs")
	}
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		fatal(err)
	}

	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	start := time.Now()
	for _, k := range []struct{ kind, table string }{
		{filmstock.KindMovie, "movies"},
		{filmstock.KindTelevision, "television_series"},
		{filmstock.KindEvent, "events"},
	} {
		n, size, err := packKind(db, *records, packDir, k.kind, k.table)
		if err != nil {
			fatal(fmt.Errorf("%s: %w", k.kind, err))
		}
		fmt.Fprintf(os.Stderr, "  %-12s %7d records  %8.1f MB  -> %s\n",
			k.kind, n, float64(size)/(1<<20), filepath.Join(packDir, k.kind+".pack"))
	}
	fmt.Fprintf(os.Stderr, "pack complete in %.1fs\n", time.Since(start).Seconds())
}

func packKind(db *sql.DB, records, packDir, kind, table string) (int, int64, error) {
	// Drive from the database, not from a directory walk: a record with no row
	// is unreachable through the API anyway, and driving from the table keeps
	// the pack and the offsets consistent by construction.
	rows, err := db.Query(`SELECT id, path FROM ` + table + ` WHERE path <> '' ORDER BY id`)
	if err != nil {
		return 0, 0, err
	}
	type item struct {
		id   int
		path string
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.path); err != nil {
			rows.Close()
			return 0, 0, err
		}
		items = append(items, it)
	}
	rows.Close()
	sort.Slice(items, func(i, j int) bool { return items[i].id < items[j].id })

	out, err := os.Create(filepath.Join(packDir, kind+".pack"))
	if err != nil {
		return 0, 0, err
	}
	defer out.Close()
	w := bufio.NewWriterSize(out, 1<<20)

	tx, err := db.Begin()
	if err != nil {
		return 0, 0, err
	}
	upd, err := tx.Prepare(`UPDATE ` + table + ` SET pack_offset = ?, pack_length = ? WHERE id = ?`)
	if err != nil {
		tx.Rollback()
		return 0, 0, err
	}

	var off int64
	for _, it := range items {
		b, err := os.ReadFile(filepath.Join(records, kind, it.path))
		if err != nil {
			// A row whose record is missing is a real inconsistency; leaving its
			// offsets null makes Remote say so instead of serving wrong bytes.
			tx.Rollback()
			return 0, 0, fmt.Errorf("id %d: %w", it.id, err)
		}
		if _, err := w.Write(b); err != nil {
			tx.Rollback()
			return 0, 0, err
		}
		if _, err := upd.Exec(off, len(b), it.id); err != nil {
			tx.Rollback()
			return 0, 0, err
		}
		off += int64(len(b))
	}
	upd.Close()
	if err := w.Flush(); err != nil {
		tx.Rollback()
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return len(items), off, nil
}
