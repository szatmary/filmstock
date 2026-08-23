package filmstock

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"sort"
	"strings"
)

func ReadTelevisionSeriesGz(path string) (*TelevisionSeries, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	data, err := io.ReadAll(gz)
	if err != nil {
		return nil, err
	}
	var s TelevisionSeries
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// TelevisionSearchResult is one ranked television series hit.
type TelevisionSearchResult struct {
	ID            int
	Title         string
	Year          int
	Network       string
	Starring      string
	Cover         string
	SeasonsCount  int
	EpisodesCount int
	Score         float64
}

// EpisodeSearchResult is one ranked episode hit.
type EpisodeSearchResult struct {
	Title          string
	SeriesID       int
	SeriesTitle    string
	Season         int
	NumberInSeason int
	AirDate        string
	Score          float64
}

// SearchEpisodes fuzzy-searches television episode titles.
func SearchEpisodes(ctx context.Context, db *sql.DB, query string, limit int) ([]EpisodeSearchResult, error) {
	qgrams := trigrams(Normalize(query))
	if len(qgrams) == 0 {
		return nil, nil
	}
	qTokens := strings.Fields(Normalize(query))
	parts := make([]string, 0, len(qgrams))
	for g := range qgrams {
		parts = append(parts, `"`+strings.ReplaceAll(g, `"`, `""`)+`"`)
	}
	// The series title comes from the join rather than a stored copy. This
	// cannot lose rows: an episode is only ever written inside the same
	// transaction as the series whose page_id it carries, so every series_id
	// has a matching row. The reindex verifies the episode count is unchanged.
	rows, err := db.QueryContext(ctx, `
		SELECT e.title,e.series_id,t.title,e.season,e.number_in_season,e.air_date
		FROM television_episodes_fts f
		JOIN television_episodes e ON e.id=f.rowid
		JOIN television_series t ON t.id=e.series_id
		WHERE television_episodes_fts MATCH ?
		ORDER BY bm25(television_episodes_fts) LIMIT 800`, strings.Join(parts, " OR "))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []EpisodeSearchResult
	for rows.Next() {
		var r EpisodeSearchResult
		var season, num sql.NullInt64
		if err := rows.Scan(&r.Title, &r.SeriesID, &r.SeriesTitle, &season, &num, &r.AirDate); err != nil {
			continue
		}
		r.Season, r.NumberInSeason = int(season.Int64), int(num.Int64)
		r.SeriesTitle = CleanTelevisionTitle(r.SeriesTitle)
		r.Score = fuzzyScore(qgrams, qTokens, r.Title)
		res = append(res, r)
	}
	sort.SliceStable(res, func(i, j int) bool { return res[i].Score > res[j].Score })
	if limit > 0 && len(res) > limit {
		res = res[:limit]
	}
	return res, nil
}

// SearchTelevision fuzzy-searches the television series index, mirroring SearchMovies.
func SearchTelevision(ctx context.Context, db *sql.DB, query, field string, limit int) ([]TelevisionSearchResult, error) {
	qgrams := trigrams(Normalize(query))
	if len(qgrams) == 0 {
		return nil, nil
	}
	qTokens := strings.Fields(Normalize(query))
	parts := make([]string, 0, len(qgrams))
	for g := range qgrams {
		parts = append(parts, `"`+strings.ReplaceAll(g, `"`, `""`)+`"`)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT t.id,t.title,t.year,t.network,t.starring,t.cover_image_file,t.seasons_count,t.episodes_count
		FROM television_fts f JOIN television_series t ON t.id=f.rowid
		WHERE television_fts MATCH ?
		ORDER BY bm25(television_fts, 10.0, 2.0, 2.0)
		LIMIT 500`, strings.Join(parts, " OR "))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []TelevisionSearchResult
	for rows.Next() {
		var r TelevisionSearchResult
		var year sql.NullInt64
		var coverFile string
		if err := rows.Scan(&r.ID, &r.Title, &year, &r.Network, &r.Starring, &coverFile, &r.SeasonsCount, &r.EpisodesCount); err != nil {
			continue
		}
		r.Year = int(year.Int64)
		r.Cover = FilePathURL(coverFile, 0)
		target := CleanTelevisionTitle(r.Title)
		if field == "starring" {
			target = r.Starring
		}
		r.Score = fuzzyScore(qgrams, qTokens, target)
		r.Title = CleanTelevisionTitle(r.Title) // display without the (TV series) suffix
		res = append(res, r)
	}
	sort.SliceStable(res, func(i, j int) bool { return res[i].Score > res[j].Score })
	if limit > 0 && len(res) > limit {
		res = res[:limit]
	}
	return res, nil
}
