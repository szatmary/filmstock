package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

// loadSeasonOf builds "episode-source page_id -> series page_id" from Wikidata's
// stated P179 ("part of the series") relation, joined back to enwiki articles
// through wiki_qid.
//
// The property is P179, NOT P4908. P4908 ("season") runs episode -> season, so
// it answers which season an episode is in; the season -> series edge this needs
// is P179. Verified against real data: "Lost, season 1" -P179-> "Lost", while
// "Homecoming" -P4908-> "Lost, season 1".
//
// This is the ONLY source of the season->series edge, by design. The obvious
// alternative — match a season article to a series by title with the
// disambiguator stripped — is precisely what lost the BBC's House of Cards, PBS
// Frontline and every non-US Big Brother: the disambiguator is the only thing
// distinguishing those shows, so removing it to build a key merges them. A
// season whose owner Wikidata does not state is left unattached and counted,
// never guessed onto a same-titled show.
func loadSeasonOf(dbPath string) (map[int]int, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("television: -wikidata is required; it supplies the " +
			"season->series edge, which cannot be derived from titles")
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	if err := requireTable(db, "wd_part_of_series"); err != nil {
		return nil, err
	}
	if err := requireTable(db, "wiki_qid"); err != nil {
		return nil, err
	}
	var hasPageID int
	db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('wiki_qid') WHERE name='page_id'`).Scan(&hasPageID)
	if hasPageID == 0 {
		return nil, fmt.Errorf("%s: wiki_qid has no page_id column; rerun build-qidmap "+
			"(Wikidata states relations between Q-ids, which only join back to articles via page_id)", dbPath)
	}

	rows, err := db.Query(`
		SELECT season.page_id, series.page_id
		FROM wd_part_of_series w
		JOIN wiki_qid season ON season.qid = w.item_qid
		JOIN wiki_qid series ON series.qid = w.series_qid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int]int{}
	for rows.Next() {
		var seasonPage, seriesPage int
		if err := rows.Scan(&seasonPage, &seriesPage); err != nil {
			return nil, err
		}
		out[seasonPage] = seriesPage
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "season->series edges from Wikidata P179: %d\n", len(out))
	return out, nil
}

func requireTable(db *sql.DB, name string) error {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name,
	).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("missing table %q; build it before parsing television", name)
	}
	return nil
}
