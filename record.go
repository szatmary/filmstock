package filmstock

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
)

// A Location identifies one record: what kind of thing it is, its identity, and
// which record in the store holds it.
//
// ID and GitdbID are different things and both are needed. ID is the identity —
// page_id for a work, Q-id for a person — and is what every other part of this
// project keys on. GitdbID is where the bytes are, allocated by the store and
// meaningful only inside it. The index maps one to the other; nothing derives
// either from the other.
type Location struct {
	Kind    string // KindMovie, KindTelevision, KindPerson, KindEvent
	ID      int    // page_id, or Q-id for people
	GitdbID uint64 // the record's id within its store
}

// A RecordSource returns the raw gzip bytes of one record.
//
// The point of the interface is that search never needs it: every list, ranking
// and count is answered from the index alone, and only opening one work costs a
// read. Dir is the implementation that ships; the interface is here so a caller
// can supply their own — an embedded FS, an object store — without this package
// having to guess which.
type RecordSource interface {
	Fetch(ctx context.Context, loc Location) ([]byte, error)
}

// decodeRecord JSON-decodes one record. The store hands back the record's bytes
// already decompressed, so there is no gzip layer here — records are stored as
// plain JSON and gitdb owns the compression.
func decodeRecord(b []byte, v any) error {
	return json.Unmarshal(b, v)
}

func ReadMovieGz(path string) (*Movie, error) {
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
	var m Movie
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}
