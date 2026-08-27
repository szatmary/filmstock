package filmstock

import (
	"database/sql"
	"fmt"
)

// FTSTables are the full-text indexes the published database DECLARES but does
// not ship populated.
//
// They are pure derived data — external-content FTS over tables the file
// already carries — and shipping them prebuilt cost 181 MB on disk and 100 MB
// of every download to save the consumer under ten seconds of local rebuild.
// Worse, a daily patch updates content tables only, so any patched consumer
// had to rebuild anyway; the prebuilt copy was stale the first time the
// database moved. The definitions travel in the schema; the content is built
// where it lives.
var FTSTables = []string{
	"movies_fts", "television_fts", "television_episodes_fts",
	"events_fts", "people_fts",
}

// RebuildFTS populates every full-text index from its content table. Run once
// after fetching the database, and after applying a patch. Seconds, not
// minutes: the trigram tokenizer builds at roughly a gigabyte a minute.
func RebuildFTS(h *sql.DB) error {
	for _, t := range FTSTables {
		// Skip tables the file does not declare: the text and vector databases
		// carry no FTS, and one rebuild call should work on any of the three.
		var n int
		if err := h.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name=?`, t).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		if _, err := h.Exec(`INSERT INTO ` + t + `(` + t + `) VALUES('rebuild')`); err != nil {
			return fmt.Errorf("filmstock: rebuilding %s: %w", t, err)
		}
	}
	return nil
}
