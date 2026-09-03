package query

import (
	"context"
	"database/sql"
	"sort"
	"strings"

	"github.com/szatmary/filmstock/internal/record"
)

// PersonResult is one ranked person hit.
type PersonResult struct {
	ID      int
	Name    string
	Credits int
	Score   float64
}

// SearchPeople fuzzy-searches the people index: FTS5 trigram retrieval, then a
// Dice re-rank on the name.
func SearchPeople(ctx context.Context, db *sql.DB, query string, limit int) ([]PersonResult, error) {
	qgrams := trigrams(record.Normalize(query))
	if len(qgrams) == 0 {
		return nil, nil
	}
	qTokens := strings.Fields(record.Normalize(query))
	parts := make([]string, 0, len(qgrams))
	for g := range qgrams {
		parts = append(parts, `"`+strings.ReplaceAll(g, `"`, `""`)+`"`)
	}
	// Retrieve candidates ranked by the FTS trigram match (bm25), then re-rank
	// by Dice in Go. Credit counts are fetched only for the final top hits.
	rows, err := db.QueryContext(ctx, `
		SELECT p.id, p.name
		FROM people_fts f JOIN people p ON p.id = f.rowid
		WHERE people_fts MATCH ?
		ORDER BY bm25(people_fts)
		LIMIT 500`, strings.Join(parts, " OR "))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type scored struct {
		PersonResult
		score float64
	}
	var cand []scored
	for rows.Next() {
		var r PersonResult
		if err := rows.Scan(&r.ID, &r.Name); err != nil {
			continue
		}
		cand = append(cand, scored{r, fuzzyScore(qgrams, qTokens, r.Name)})
	}
	sort.SliceStable(cand, func(i, j int) bool { return cand[i].score > cand[j].score })
	if limit > 0 && len(cand) > limit {
		cand = cand[:limit]
	}
	out := make([]PersonResult, len(cand))
	for i, c := range cand {
		c.Credits = CountCredits(ctx, db, c.ID)
		c.PersonResult.Score = c.score
		out[i] = c.PersonResult
	}
	return out, nil
}

func CountCredits(ctx context.Context, db *sql.DB, personID int) int {
	var n int
	db.QueryRowContext(ctx, `SELECT count(*) FROM credits WHERE person_id = ?`, personID).Scan(&n)
	return n
}

// Filmography is a person's credits grouped by role, for the person page.
type Filmography struct {
	ID    int
	Name  string
	Image string
	Roles []RoleGroup
}

type RoleGroup struct {
	Role   string
	Movies []CreditMovie
}

type CreditMovie struct {
	ID    int
	Title string
	Year  int
	Type  string // "movie" or "television"
}

// roleOrder controls how credit sections are ordered on a person page.
var roleOrder = map[string]int{
	"Director": 0, "Writer": 1, "Creator": 2, "Producer": 3,
	"Executive Producer": 4, "Cast": 5, "Presenter": 6, "Narrator": 7,
	"Composer": 8, "Cinematographer": 9, "Editor": 10, "Host": 11,
}

// PersonFilmography loads a person by id and returns their credits (movies AND
// television) grouped by role, each role's works sorted newest-first.
func PersonFilmography(db *sql.DB, id int) (*Filmography, error) {
	var name string
	if err := db.QueryRow(`SELECT name FROM people WHERE id = ?`, id).Scan(&name); err != nil {
		return nil, err
	}
	rows, err := db.Query(`
		SELECT c.role, c.work_type,
		       CASE c.work_type WHEN 'movie' THEN m.title ELSE t.title END AS title,
		       CASE c.work_type WHEN 'movie' THEN m.year  ELSE t.year  END AS year,
		       c.work_id
		FROM credits c
		LEFT JOIN movies m    ON c.work_type='movie' AND m.id = c.work_id
		LEFT JOIN television_series t ON c.work_type='television'    AND t.id = c.work_id
		WHERE c.person_id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byRole := map[string][]CreditMovie{}
	seen := map[string]bool{}
	for rows.Next() {
		var role string
		var cm CreditMovie
		var title sql.NullString
		var year sql.NullInt64
		if err := rows.Scan(&role, &cm.Type, &title, &year, &cm.ID); err != nil {
			continue
		}
		if !title.Valid || title.String == "" {
			continue // work row missing (e.g. television table absent)
		}
		k := role + "\x00" + cm.Type + "\x00" + title.String
		if seen[k] {
			continue
		}
		seen[k] = true
		cm.Title = record.CleanTitle(title.String)
		cm.Year = int(year.Int64)
		byRole[role] = append(byRole[role], cm)
	}

	fm := &Filmography{ID: id, Name: name}
	for role, movies := range byRole {
		sort.SliceStable(movies, func(i, j int) bool { return movies[i].Year > movies[j].Year })
		fm.Roles = append(fm.Roles, RoleGroup{Role: role, Movies: movies})
	}
	sort.SliceStable(fm.Roles, func(i, j int) bool {
		return roleOrder[fm.Roles[i].Role] < roleOrder[fm.Roles[j].Role]
	})
	return fm, nil
}

// PersonIDByName resolves an exact person name to its id (0 if not found).
func PersonIDByName(db *sql.DB, name string) int {
	var id int
	db.QueryRow(`SELECT id FROM people WHERE name = ? ORDER BY id LIMIT 1`, name).Scan(&id)
	return id
}

// PersonIDByWiki resolves a wiki link target to its person id via person_alias
// (the canonical Q-id-backed identity).
func PersonIDByWiki(db *sql.DB, wiki string) int {
	var id int
	db.QueryRow(`SELECT person_id FROM person_alias WHERE wiki = ?`, wiki).Scan(&id)
	return id
}
