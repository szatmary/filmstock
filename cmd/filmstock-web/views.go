package main

import (
	"context"
	"strings"
)

// The record store is gone: every page is built from the published SQLite
// columns alone. List-ish columns are " · "-joined display strings; they are
// split back into slices only where a template links each element.

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, " · ")
}

type movieView struct {
	ID          int
	Title       string
	Year        int
	ReleaseDate string
	Director    string
	Producer    string
	Writer      string
	Music       string
	Starring    []string
	Distributor string
	Country     string
	Language    string
	Genre       string
	Runtime     string
	Budget      string
	Gross       string

	CoverImageURL string
	WikiURL       string
	Overview      string
	Plot          string
}

func (s *server) movieView(ctx context.Context, id int) (*movieView, error) {
	m := &movieView{ID: id}
	var starring string
	err := s.fs.SQL().QueryRowContext(ctx, `
		SELECT title, COALESCE(year,0), COALESCE(release_date,''),
		       COALESCE(director,''), COALESCE(producer,''), COALESCE(writer,''),
		       COALESCE(starring,''), COALESCE(music,''), COALESCE(distributor,''),
		       COALESCE(country,''), COALESCE(language,''), COALESCE(genre,''),
		       COALESCE(runtime,''), COALESCE(budget,''), COALESCE(gross,''),
		       COALESCE(cover_image_url,''), COALESCE(wikipedia_url,'')
		FROM movies WHERE id = ?`, id).Scan(
		&m.Title, &m.Year, &m.ReleaseDate,
		&m.Director, &m.Producer, &m.Writer,
		&starring, &m.Music, &m.Distributor,
		&m.Country, &m.Language, &m.Genre,
		&m.Runtime, &m.Budget, &m.Gross,
		&m.CoverImageURL, &m.WikiURL)
	if err != nil {
		return nil, err
	}
	m.Starring = splitList(starring)
	m.Overview, m.Plot = s.textOf("movie_text", id)
	return m, nil
}

type episodeView struct {
	NumberInSeason int
	NumberOverall  int
	Title          string
	AirDate        string
	Summary        string
}

type seasonView struct {
	Season      int
	NumEpisodes int
	FirstAired  string
	Episodes    []episodeView
}

type televisionView struct {
	ID          int
	Title       string
	FirstAired  string
	LastAired   string
	NumSeasons  string
	NumEpisodes string
	Network     string
	Genre       string
	Creator     string
	Starring    string

	CoverImageURL string
	WikiURL       string
	Overview      string
	Plot          string
	Seasons       []seasonView
}

func (s *server) televisionView(ctx context.Context, id int) (*televisionView, error) {
	t := &televisionView{ID: id}
	err := s.fs.SQL().QueryRowContext(ctx, `
		SELECT title, COALESCE(first_aired,''), COALESCE(last_aired,''),
		       COALESCE(num_seasons,''), COALESCE(num_episodes,''),
		       COALESCE(network,''), COALESCE(genre,''), COALESCE(creator,''),
		       COALESCE(starring,''), COALESCE(cover_image_url,''),
		       COALESCE(wikipedia_url,'')
		FROM television_series WHERE id = ?`, id).Scan(
		&t.Title, &t.FirstAired, &t.LastAired,
		&t.NumSeasons, &t.NumEpisodes,
		&t.Network, &t.Genre, &t.Creator,
		&t.Starring, &t.CoverImageURL, &t.WikiURL)
	if err != nil {
		return nil, err
	}
	t.Overview, t.Plot = s.textOf("television_text", id)

	seasons := map[int]*seasonView{}
	var order []int
	rows, err := s.fs.SQL().QueryContext(ctx, `
		SELECT season, COALESCE(num_episodes,0), COALESCE(first_aired,'')
		FROM television_seasons WHERE series_id = ? ORDER BY season`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		sv := &seasonView{}
		if err := rows.Scan(&sv.Season, &sv.NumEpisodes, &sv.FirstAired); err != nil {
			rows.Close()
			return nil, err
		}
		seasons[sv.Season] = sv
		order = append(order, sv.Season)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.fs.SQL().QueryContext(ctx, `
		SELECT id, COALESCE(season,0), COALESCE(number_in_season,0),
		       COALESCE(number_overall,0), COALESCE(title,''), COALESCE(air_date,'')
		FROM television_episodes WHERE series_id = ?
		ORDER BY season, number_overall, number_in_season`, id)
	if err != nil {
		return nil, err
	}
	epSeason := []int{}
	epRow := []int64{}
	for rows.Next() {
		var rowid int64
		var season int
		var ep episodeView
		if err := rows.Scan(&rowid, &season, &ep.NumberInSeason,
			&ep.NumberOverall, &ep.Title, &ep.AirDate); err != nil {
			rows.Close()
			return nil, err
		}
		sv := seasons[season]
		if sv == nil {
			sv = &seasonView{Season: season}
			seasons[season] = sv
			order = append(order, season)
		}
		sv.Episodes = append(sv.Episodes, ep)
		epSeason = append(epSeason, season)
		epRow = append(epRow, rowid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Episode summaries live in the text database, keyed by episode rowid.
	if s.text != nil && len(epRow) > 0 {
		sums := map[int64]string{}
		srows, err := s.text.QueryContext(ctx,
			`SELECT id, COALESCE(summary,'') FROM episode_text WHERE series_id = ?`, id)
		if err == nil {
			for srows.Next() {
				var id int64
				var sum string
				if srows.Scan(&id, &sum) == nil {
					sums[id] = sum
				}
			}
			srows.Close()
		}
		idx := map[int]int{}
		for i, season := range epSeason {
			sv := seasons[season]
			sv.Episodes[idx[season]].Summary = sums[epRow[i]]
			idx[season]++
		}
	}

	for _, n := range order {
		t.Seasons = append(t.Seasons, *seasons[n])
	}
	return t, nil
}

type eventView struct {
	ID       int
	Title    string
	Kind     string
	Award    string
	Edition  int
	Date     string
	Hosts    []string
	Network  string
	Venue    string
	Location string

	BestFilm    string
	MostWins    string
	OpeningFilm string
	ClosingFilm string
	WikiURL     string
}

func (s *server) eventView(ctx context.Context, id int) (*eventView, error) {
	e := &eventView{ID: id}
	var hosts string
	err := s.fs.SQL().QueryRowContext(ctx, `
		SELECT title, kind, COALESCE(award,''), COALESCE(edition,0),
		       COALESCE(date,''), COALESCE(hosts,''), COALESCE(network,''),
		       COALESCE(venue,''), COALESCE(location,''),
		       COALESCE(best_film,''), COALESCE(most_wins,''),
		       COALESCE(opening_film,''), COALESCE(closing_film,''),
		       COALESCE(wikipedia_url,'')
		FROM events WHERE id = ?`, id).Scan(
		&e.Title, &e.Kind, &e.Award, &e.Edition,
		&e.Date, &hosts, &e.Network,
		&e.Venue, &e.Location,
		&e.BestFilm, &e.MostWins,
		&e.OpeningFilm, &e.ClosingFilm, &e.WikiURL)
	if err != nil {
		return nil, err
	}
	e.Hosts = splitList(hosts)
	return e, nil
}

// textOf reads one row of a text-database table; absent text is "".
func (s *server) textOf(table string, id int) (overview, plot string) {
	if s.text == nil {
		return "", ""
	}
	err := s.text.QueryRow(
		`SELECT COALESCE(overview,''), COALESCE(plot,'') FROM `+table+` WHERE id = ?`,
		id).Scan(&overview, &plot)
	if err != nil {
		return "", ""
	}
	return overview, plot
}
