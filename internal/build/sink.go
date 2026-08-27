package build

import "encoding/json"

// A recordSink takes finished records. The record builder needs three things
// from it and no more, which is what makes the storage format replaceable: the
// gitdb tree and the published SQLite database are both just sinks.
type recordSink interface {
	// put stores one record under its identity, which is always an enwiki
	// page_id.
	put(kind string, identity int64, v any)
	// sweep removes whatever a complete run did not write, and reports how
	// many. A partial run must not call it.
	sweep() int
	// Counts reports what the run left alone, so an ingest can say how much of
	// the store it did not touch.
	Counts() (unchanged, updated, inserted int)
	// Err reports the first failure, if any.
	Err() error
}

// identityOf reads the identity out of a record's own bytes: the enwiki
// page_id, for every kind, people included. A record without one is not an
// entity. The live pipeline enforces this at each put() call site in
// extract.go; this states the rule as one function so the identity tests can
// pin it — the doctrine that once broke as 85,523 people keyed by a hash of
// their article path.
func identityOf(kind string, data []byte) (int64, bool) {
	var probe struct {
		PageID int64 `json:"page_id"`
	}
	if json.Unmarshal(data, &probe) != nil {
		return 0, false
	}
	_ = kind // the rule is the same for every kind; the parameter keeps call sites honest
	return probe.PageID, probe.PageID != 0
}
