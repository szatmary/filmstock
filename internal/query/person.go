package query

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/szatmary/filmstock/internal/record"
)

// PersonByID returns the record for a person: their identity, and — when their
// credit links to an article that is actually a biography — birth and death,
// occupation, nationality and the lead of their article.
//
// Not every person has one. A credit's link target may be a redlink, a
// disambiguation page, or something that is not a person at all; those return a
// record with identity and no PersonBio, which is the honest answer rather than
// an error.
func PersonByID(ctx context.Context, db *sql.DB, id int) (*record.PersonRecord, error) {
	var qid, pageID sql.NullInt64
	var name, wiki string
	err := db.QueryRowContext(ctx,
		`SELECT page_id, qid, name, COALESCE(wiki,'') FROM people WHERE id = ?`,
		id).Scan(&pageID, &qid, &name, &wiki)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("no person with id %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	rec := &record.PersonRecord{PageID: int(pageID.Int64), QID: qid.Int64, Wiki: wiki, Name: name}
	return rec, nil
}
