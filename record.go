package filmstock

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// A Location identifies one record: what kind of thing it is, its id, and where
// the index says its file lives.
type Location struct {
	Kind string // KindMovie, KindTelevision, KindEvent
	ID   int
	Path string // shard-relative, e.g. "a2/3746.json.gz"
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

// Dir serves records from an extracted output tree — the layout `filmstock
// extract` writes, and what you want for local development and tests.
func Dir(root string) RecordSource { return dirSource{root} }

type dirSource struct{ root string }

func (d dirSource) Fetch(_ context.Context, loc Location) ([]byte, error) {
	if loc.Path == "" {
		return nil, fmt.Errorf("filmstock: %s %d has no record path", loc.Kind, loc.ID)
	}
	return os.ReadFile(filepath.Join(d.root, loc.Kind, loc.Path))
}

// decodeRecord un-gzips and JSON-decodes one record's bytes into v.
func decodeRecord(b []byte, v any) error {
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer gz.Close()
	data, err := io.ReadAll(gz)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
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
