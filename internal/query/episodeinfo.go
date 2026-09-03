package query

import (
	"context"
	"database/sql"
	"fmt"
)

// EpisodeInfo is one episode as a list row: enough to draw an episode guide,
// nothing that needs the full series record.
type EpisodeInfo struct {
	Season         int    `json:"season"`
	NumberInSeason int    `json:"number_in_season,omitempty"`
	Title          string `json:"title"`
	AirDate        string `json:"air_date,omitempty"`

	// ProdCode is the production code the row states, opaque and per-series;
	// see Episode.ProdCode. It is carried here because for a multi-part episode
	// it is the only field that separates the parts: Futurama season 5 lists
	// four rows titled "Bender's Big Score", all dated 2007-11-27, which differ
	// only as 5ACV01, 5ACV02, 5ACV03, 5ACV04.
	ProdCode string `json:"prod_code,omitempty"`
}

// Episodes returns every episode of one series, ordered by season and then
// by number within the season. seriesID is the series page_id, as everywhere.
//
// An empty result is ordinary, not an error: plenty of series have no
// episode-list article, and this call cannot tell that apart from a series id
// the database does not hold — SeriesInfo answers whether the series exists.
func Episodes(ctx context.Context, db *sql.DB, seriesID int) ([]EpisodeInfo, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT season, number_in_season, title, air_date, prod_code
		 FROM television_episodes WHERE series_id = ?
		 ORDER BY season, number_in_season, number_overall, id`, seriesID)
	if err != nil {
		return nil, fmt.Errorf("filmstock: episode info for series %d: %w", seriesID, err)
	}
	defer rows.Close()
	var out []EpisodeInfo
	for rows.Next() {
		var e EpisodeInfo
		var season, num sql.NullInt64
		var title, air, prod sql.NullString
		if err := rows.Scan(&season, &num, &title, &air, &prod); err != nil {
			return nil, err
		}
		e.Season, e.NumberInSeason = int(season.Int64), int(num.Int64)
		e.Title, e.AirDate, e.ProdCode = title.String, air.String, prod.String
		out = append(out, e)
	}
	return out, rows.Err()
}
