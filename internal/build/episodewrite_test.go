package build

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/szatmary/filmstock/internal/record"
	"github.com/szatmary/filmstock/internal/sqldrv"
)

// The parse and the schema have to agree, and nothing else in the suite writes
// an episode through the real DDL. This walks one series from a parsed record
// to a row read back out, so a column added to Episode but not to the INSERT
// (or vice versa) fails here rather than in a six-hour build.
func TestEpisodeRoundTripsThroughTheDatabase(t *testing.T) {
	dir := t.TempDir()
	w, err := newDBWriter(filepath.Join(dir, "core.db"), filepath.Join(dir, "text.db"))
	if err != nil {
		t.Fatal(err)
	}
	w.put(record.KindTelevision, 228211, &record.TelevisionSeries{
		Title:  "Futurama",
		PageID: 228211,
		Type:   "television",
		Seasons: []*record.Season{{
			Season: 5,
			Episodes: []*record.Episode{
				{NumberOverall: 73, NumberInSeason: 1, Title: "Bender's Big Score",
					AirDate: "2008-03-23", ProdCode: "5ACV01", Summary: "Part one."},
				{NumberOverall: 74, NumberInSeason: 2, Title: "Bender's Big Score",
					AirDate: "2008-03-23", ProdCode: "5ACV02"},
				// No production code: the column must be readable as empty
				// rather than tripping the scan.
				{NumberOverall: 75, NumberInSeason: 3, Title: "Bender's Big Score",
					AirDate: "2008-03-23"},
			},
		}},
	})
	if err := w.Err(); err != nil {
		t.Fatal(err)
	}
	if err := w.commit(); err != nil {
		t.Fatal(err)
	}
	w.db.Close()

	db, err := sql.Open(sqldrv.Name, filepath.Join(dir, "core.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT number_in_season, title, air_date,
	   COALESCE(prod_code,'') FROM television_episodes
	   WHERE series_id=228211 ORDER BY number_in_season`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row struct {
		num              int
		title, air, prod string
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.num, &r.title, &r.air, &r.prod); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []row{
		{1, "Bender's Big Score", "2008-03-23", "5ACV01"},
		{2, "Bender's Big Score", "2008-03-23", "5ACV02"},
		{3, "Bender's Big Score", "2008-03-23", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("wrote %d episodes, read back %d", len(want), len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("episode %d: %+v, want %+v", want[i].num, got[i], want[i])
		}
	}

	// The three rows share a title and an air date, so if the id folded in the
	// production code the one without a code would still be distinct — but the
	// numbers already separate them, and the id must not have changed shape.
	var ids int
	if err := db.QueryRow(`SELECT COUNT(DISTINCT id) FROM television_episodes
	   WHERE series_id=228211`).Scan(&ids); err != nil {
		t.Fatal(err)
	}
	if ids != 3 {
		t.Errorf("%d distinct episode ids, want 3", ids)
	}
}
