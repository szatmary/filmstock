package filmstock

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

func ReadEventGz(path string) (*Event, error) {
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
	var e Event
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// SearchEvents ranks ceremonies and festivals with the same trigram retrieve +
// Dice re-rank the other searchers use, so a typo costs no more here than it
// does on a film title.
func SearchEvents(ctx context.Context, db *sql.DB, query string, limit int) ([]UnifiedResult, error) {
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
		SELECT e.id, e.title, e.kind, e.year, coalesce(e.award,''), coalesce(e.hosts,'')
		FROM events_fts f JOIN events e ON e.id = f.rowid
		WHERE events_fts MATCH ?
		ORDER BY bm25(events_fts, 10.0, 3.0, 2.0)
		LIMIT 400`, strings.Join(parts, " OR "))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UnifiedResult{}
	for rows.Next() {
		var id int
		var year sql.NullInt64
		var title, kind, award, hosts string
		if err := rows.Scan(&id, &title, &kind, &year, &award, &hosts); err != nil {
			continue
		}
		sub := award
		if hosts != "" {
			if sub != "" {
				sub += " · "
			}
			sub += "hosted by " + hosts
		}
		out = append(out, UnifiedResult{
			Type:     "event",
			Title:    title,
			Subtitle: sub,
			Link:     fmt.Sprintf("/event?id=%d", id),
			Score:    fuzzyScore(qgrams, qTokens, title),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
