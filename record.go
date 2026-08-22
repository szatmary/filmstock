package filmstock

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A Location identifies one record. It is whatever the database knows about
// where a record lives, and it is deliberately enough for BOTH kinds of source:
// Path answers "which file", Offset/Length answer "which bytes of which pack".
// A source uses the fields it needs and ignores the rest.
type Location struct {
	Kind   string // KindMovie, KindTelevision, KindEvent
	ID     int
	Path   string // shard-relative, e.g. "a2/3746.json.gz"
	Offset int64  // byte offset into <Kind>.pack
	Length int64  // bytes; 0 means the pack columns were never populated
}

// A RecordSource returns the raw gzip bytes of one record.
//
// The whole point of the interface is that search never needs it: every list,
// ranking and count is answered from the database alone. Only opening a single
// work costs a fetch, which is what makes a 161 MB download plus on-demand
// detail a sensible way to ship this.
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

// Remote serves records out of per-kind pack files under baseURL, one HTTP
// range request each — e.g. a GitHub release whose assets are movies.pack,
// television.pack and events.pack.
//
// Packs are split by kind rather than combined into one file for two reasons: a
// single pack would sit uncomfortably close to GitHub's 2 GB per-asset limit and
// grow with every ingest, and a consumer who only wants films should never pay
// for the television bytes.
func Remote(baseURL string) RecordSource {
	return &httpSource{
		base:   strings.TrimRight(baseURL, "/"),
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// RemoteWithClient is Remote with a caller-supplied client, for callers that
// need their own timeouts, transport, retries or auth.
func RemoteWithClient(baseURL string, c *http.Client) RecordSource {
	return &httpSource{base: strings.TrimRight(baseURL, "/"), client: c}
}

type httpSource struct {
	base   string
	client *http.Client
}

func (h *httpSource) Fetch(ctx context.Context, loc Location) ([]byte, error) {
	// Length 0 means nobody ran `filmstock pack` against this database. Say so,
	// rather than issuing a range request that quietly returns nothing useful.
	if loc.Length <= 0 {
		return nil, fmt.Errorf("filmstock: %s %d has no pack offset; this database "+
			"was not packed for remote use (run `filmstock pack`)", loc.Kind, loc.ID)
	}
	url := h.base + "/" + loc.Kind + ".pack"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", loc.Offset, loc.Offset+loc.Length-1))
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// A 200 means the server ignored Range and is sending the whole pack. Refuse
	// it: silently streaming 1.2 GB to answer a 5 KB lookup is worse than failing.
	if resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("filmstock: %s: want 206 Partial Content, got %s "+
			"(does the host honour Range?)", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, loc.Length))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != loc.Length {
		return nil, fmt.Errorf("filmstock: %s %d: short read, %d of %d bytes",
			loc.Kind, loc.ID, len(b), loc.Length)
	}
	return b, nil
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
