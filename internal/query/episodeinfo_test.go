package query

import (
	"context"
	"database/sql"
	"testing"

	"github.com/szatmary/filmstock/internal/sqldrv"
)

func episodeTestDB(t *testing.T) *sql.DB {
	t.Helper()
	h, err := sql.Open(sqldrv.Name, "file:"+t.TempDir()+"/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	for _, q := range []string{
		`CREATE TABLE television_episodes(id INTEGER PRIMARY KEY AUTOINCREMENT,
		   series_id INTEGER, season INTEGER,
		   number_in_season INTEGER, number_overall INTEGER, title TEXT, air_date TEXT,
		   viewers REAL, prod_code TEXT)`,
		// Inserted out of order on purpose: the ORDER BY is the contract.
		`INSERT INTO television_episodes(series_id, season, number_in_season, number_overall, title, air_date) VALUES
		   (100, 2, 1, 8, 'Seven Thirty-Seven', '2009-03-08'),
		   (100, 1, 2, 2, 'Cat''s in the Bag...', '2008-01-27'),
		   (100, 1, 1, 1, 'Pilot', '2008-01-20'),
		   (999, 1, 1, 1, 'Some Other Pilot', '1999-01-01')`,
	} {
		if _, err := h.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// Episodes come back in airing order — season, then number within it — no
// matter the insertion order, and only for the series asked.
func TestEpisodeInfoOrder(t *testing.T) {
	db := episodeTestDB(t)
	got, err := Episodes(context.Background(), db, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []EpisodeInfo{
		{Season: 1, NumberInSeason: 1, Title: "Pilot", AirDate: "2008-01-20"},
		{Season: 1, NumberInSeason: 2, Title: "Cat's in the Bag...", AirDate: "2008-01-27"},
		{Season: 2, NumberInSeason: 1, Title: "Seven Thirty-Seven", AirDate: "2009-03-08"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d episodes, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("episode %d: %+v, want %+v", i, got[i], w)
		}
	}
}

// A series the database does not hold yields an empty list, not an error.
func TestEpisodeInfoUnknownSeries(t *testing.T) {
	db := episodeTestDB(t)
	got, err := Episodes(context.Background(), db, 12345)
	if err != nil || got != nil {
		t.Fatalf("got %v, %v", got, err)
	}
}
