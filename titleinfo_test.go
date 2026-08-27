package filmstock

import (
	"context"
	"database/sql"
	"testing"

	"github.com/szatmary/filmstock/internal/sqldrv"
)

func infoTestDB(t *testing.T) *DB {
	t.Helper()
	h, err := sql.Open(sqldrv.Name, "file:"+t.TempDir()+"/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	for _, q := range []string{
		`CREATE TABLE movies(id INTEGER PRIMARY KEY, title TEXT, year INTEGER,
		   cover_image_file TEXT, cover_image_url TEXT, wikipedia_url TEXT)`,
		`CREATE TABLE television_series(id INTEGER PRIMARY KEY, title TEXT, year INTEGER,
		   cover_image_file TEXT, cover_image_url TEXT, wikipedia_url TEXT)`,
		`INSERT INTO movies VALUES
		   (3746,'Blade Runner',1982,'poster.png','https://upload.example/p.png','https://en.wikipedia.org/wiki/Blade_Runner'),
		   (1,'Heat',1995,'','',''),
		   (2,'Alien',1979,'a.jpg','https://upload.example/a.jpg','')`,
		`INSERT INTO television_series VALUES
		   (8209,'Doctor Who',1963,'','','https://en.wikipedia.org/wiki/Doctor_Who')`,
	} {
		if _, err := h.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return FromSQL(h)
}

// Results come back in the order asked, and a missing id is absent, not an
// error: a caller holding ids from an older build should still get the rest.
func TestFilmInfoOrderAndMisses(t *testing.T) {
	db := infoTestDB(t)
	got, err := db.FilmInfo(context.Background(), []int{2, 99999, 3746, 1})
	if err != nil {
		t.Fatal(err)
	}
	want := []int{2, 3746, 1}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].ID != w {
			t.Errorf("row %d: id %d, want %d — order must follow the request", i, got[i].ID, w)
		}
	}
	if got[1].Title != "Blade Runner" || got[1].Year != 1982 ||
		got[1].CoverImageURL != "https://upload.example/p.png" {
		t.Errorf("Blade Runner row wrong: %+v", got[1])
	}
}

// A duplicated id is returned once: the caller asked about a set, however
// sloppily, and doubling a card is never what a grid wants.
func TestFilmInfoDeduplicates(t *testing.T) {
	db := infoTestDB(t)
	got, err := db.FilmInfo(context.Background(), []int{1, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows for one film asked three times", len(got))
	}
}

func TestSeriesInfo(t *testing.T) {
	db := infoTestDB(t)
	got, err := db.SeriesInfo(context.Background(), []int{8209})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "Doctor Who" || got[0].Year != 1963 {
		t.Fatalf("got %+v", got)
	}
}

// Empty in, empty out, no query.
func TestInfoEmpty(t *testing.T) {
	db := infoTestDB(t)
	if got, err := db.FilmInfo(context.Background(), nil); err != nil || got != nil {
		t.Fatalf("got %v, %v", got, err)
	}
}

// More ids than one IN list can bind: the chunking must be invisible.
func TestInfoChunks(t *testing.T) {
	db := infoTestDB(t)
	ids := make([]int, 0, 1203)
	for i := 0; i < 1200; i++ {
		ids = append(ids, 100000+i)
	}
	ids = append(ids, 3746, 1, 2)
	got, err := db.FilmInfo(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows through the chunk boundary, want 3", len(got))
	}
}
