package build

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The synopses live in their own database file.
//
// They are 462 MB of the 974 MB total — film plots, series plots, and 583,401
// episode summaries — and they are the half of the data a consumer is most
// likely not to want. A media application needs titles, credits, air dates and
// schedules; it does not need every plot to answer "what did this person appear
// in". So the core database stays 409 MB (204 compressed) and the synopses are
// an extra 462 MB for whoever wants them.
//
// Split rather than compressed. Compression would take the same 462 MB down to
// roughly 120, but everyone would still download it; a separate file is the
// difference between paying for it and not. The two are not exclusive — the
// shipped files can still be compressed for transport.
//
// Keyed by exactly the ids in the core database, so the two attach and join:
//
//	ATTACH 'filmstock-text.db' AS t;
//	SELECT m.title, t.plot FROM movies m JOIN t.movie_text t ON t.id = m.id;
const textSchema = `
CREATE TABLE IF NOT EXISTS movie_text(
  id INTEGER PRIMARY KEY, overview TEXT, plot TEXT
);
CREATE TABLE IF NOT EXISTS television_text(
  id INTEGER PRIMARY KEY, overview TEXT, plot TEXT
);
-- Keyed by the episode's rowid in the core database's television_episodes,
-- which is an AUTOINCREMENT id rather than anything the encyclopaedia states.
-- That makes this file only meaningful beside the core database it was built
-- with, which is why both carry the same build fingerprint.
CREATE TABLE IF NOT EXISTS episode_text(
  id INTEGER PRIMARY KEY, series_id INTEGER, summary TEXT
);
CREATE INDEX IF NOT EXISTS idx_episode_text_series ON episode_text(series_id);
`

// attachText opens the synopsis database alongside an open core database, so
// both are written in one transaction and cannot disagree about what exists.
func attachText(db *sql.DB, path string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return err
		}
	}
	// A single-quoted SQL literal: the path is ours, but doubling quotes costs
	// nothing and keeps a directory with an apostrophe from becoming a syntax
	// error at the worst moment.
	lit := "'" + strings.ReplaceAll(path, "'", "''") + "'"
	if _, err := db.Exec(`ATTACH DATABASE ` + lit + ` AS synopsis`); err != nil {
		return fmt.Errorf("attach %s: %w", path, err)
	}
	if _, err := db.Exec(strings.ReplaceAll(textSchema, "IF NOT EXISTS ", "IF NOT EXISTS synopsis.")); err != nil {
		return fmt.Errorf("synopsis schema: %w", err)
	}
	return nil
}

// defaultTextPath puts the synopses beside the database they belong to.
func defaultTextPath(dbPath string) string {
	ext := filepath.Ext(dbPath)
	return strings.TrimSuffix(dbPath, ext) + "-text" + ext
}
