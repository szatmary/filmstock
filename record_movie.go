package filmstock

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
)

// ReadMovieGz reads and decodes one gzip-compressed movie JSON file.
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
