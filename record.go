package filmstock

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"strconv"
)

// A Location identifies one record: what kind of thing it is, and its identity.
//
// Format 5 stores allocated their own record ids, so a Location had to carry
// both the identity and that id, and the index existed partly to map one to the
// other. Format 6 keys records by the identity itself, so there is nothing left
// to map: ID is the key.
type Location struct {
	Kind string // KindMovie, KindTelevision, KindPerson, KindEvent
	ID   int    // page_id, or Q-id for people
}

// StoreKey renders an identity as the key its record is stored under. Person
// identities go negative when the person has no Q-id, which is why this is
// signed; the key just has to be stable and free of whitespace.
func StoreKey(identity int64) string { return strconv.FormatInt(identity, 10) }

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
