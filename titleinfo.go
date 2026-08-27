package filmstock

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// TitleInfo is the little a UI needs to draw a work in a list or grid: enough
// to render a card, nothing that needs the full record.
type TitleInfo struct {
	ID             int    `json:"id"` // the enwiki page_id, as everywhere
	Title          string `json:"title"`
	Year           int    `json:"year,omitempty"`
	CoverImageFile string `json:"cover_image_file,omitempty"`
	// CoverImageURL is the direct upload.wikimedia.org URL, ready to put in an
	// img tag; the file name is kept beside it for callers that render their
	// own paths or cache by name.
	CoverImageURL string `json:"cover_image_url,omitempty"`
	WikiURL       string `json:"wikipedia_url,omitempty"`
}

// FilmInfo returns card-level metadata for a batch of films.
//
// A batch, because the caller drawing a grid of forty posters should cost one
// query, not forty. Results come back in the ORDER ASKED, and an id the
// database does not hold is simply absent — a missing film is an ordinary
// condition for a caller holding ids from an older build, not an error that
// should abort the other thirty-nine.
func (db *DB) FilmInfo(ctx context.Context, ids []int) ([]TitleInfo, error) {
	return db.titleInfo(ctx, "movies", ids)
}

// SeriesInfo is FilmInfo for television.
func (db *DB) SeriesInfo(ctx context.Context, ids []int) ([]TitleInfo, error) {
	return db.titleInfo(ctx, "television_series", ids)
}

func (db *DB) titleInfo(ctx context.Context, table string, ids []int) ([]TitleInfo, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	found := make(map[int]TitleInfo, len(ids))
	// Chunked so a caller with thousands of ids does not hit SQLite's bound
	// variable limit; 500 is comfortably under every build's default.
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(start+500, len(ids))]
		ph := strings.Repeat("?,", len(chunk))
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := db.sql.QueryContext(ctx,
			`SELECT id, title, year, cover_image_file, cover_image_url, wikipedia_url
			 FROM `+table+` WHERE id IN (`+ph[:len(ph)-1]+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("filmstock: %s info: %w", table, err)
		}
		for rows.Next() {
			var t TitleInfo
			var year sql.NullInt64
			var file, url, wiki sql.NullString
			if err := rows.Scan(&t.ID, &t.Title, &year, &file, &url, &wiki); err != nil {
				rows.Close()
				return nil, err
			}
			t.Year = int(year.Int64)
			t.CoverImageFile, t.CoverImageURL, t.WikiURL = file.String, url.String, wiki.String
			found[t.ID] = t
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	out := make([]TitleInfo, 0, len(found))
	seen := make(map[int]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if t, ok := found[id]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}
