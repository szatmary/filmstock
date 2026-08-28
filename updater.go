package filmstock

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/szatmary/filmstock/internal/sqldrv"
)

// Keeping a running consumer current.
//
// The published data is a catalog of builds behind a base URL. An Updater
// checks that catalog, fetches what is newer than what it holds, verifies the
// bytes against the manifest, rebuilds the local search indexes, and hands the
// result over as an atomic swap — a serving process never closes its database
// to take an update, it opens the new one beside the old and flips a pointer.
//
// Builds land in their own directories, so an interrupted download can never
// damage the build being served:
//
//	<dir>/20260801/filmstock.db     the build in use
//	<dir>/20260820/…                arriving; invisible until verified
//	<dir>/state.json                which build is current
//
// The cheap path is the patch chain: when the catalog shows an unbroken line
// of parents from what is held to the latest build — dailies naming their
// parent, a full naming the chain it bridges from — the updater copies the
// current files forward, applies each build's patches in order, and demands
// the final content hashes match the target's manifest. Any break, any
// missing patch, any mismatch, and it falls back to downloading the build
// whole; the content hash is what proves the two paths converge.
type Updater struct {
	// BaseURL serves builds.json, e.g. "https://dl.example.org/filmstock".
	// A value with no scheme is a LOCAL DIRECTORY laid out the same way —
	// "fake it until the bucket exists": point at a directory today, swap one
	// string for the real URL later, and nothing else changes.
	BaseURL string
	// Dir is where builds live locally.
	Dir string
	// Files is which of a build's artifacts to fetch. Nil means just the core
	// database; add filmstock-text.db or filmstock-vectors.db as wanted.
	Files []string
	// VerifyContent re-computes the content hash after the FTS rebuild and
	// refuses the build on mismatch. ~20 s of paranoia per update; on by
	// default via NewUpdater because a wrong database that opens cleanly is
	// worse than a slow one.
	VerifyContent bool
	// Client, for tests and proxies. Nil means a client with sane timeouts.
	Client *http.Client
}

func NewUpdater(baseURL, dir string) *Updater {
	return &Updater{BaseURL: baseURL, Dir: dir, VerifyContent: true}
}

func (u *Updater) files() []string {
	if len(u.Files) > 0 {
		return u.Files
	}
	return []string{"filmstock.db"}
}

func (u *Updater) client() *http.Client {
	if u.Client != nil {
		return u.Client
	}
	return &http.Client{Timeout: 30 * time.Minute}
}

type updaterState struct {
	Current string `json:"current"`
}

type catalogEntry struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Parent     string `json:"parent"`
	BridgeFrom string `json:"bridge_from"`
}
type catalog struct {
	LatestFull string         `json:"latest_full"`
	Latest     string         `json:"latest"`
	Builds     []catalogEntry `json:"builds"`
}

func (c *catalog) entry(id string) *catalogEntry {
	for i := range c.Builds {
		if c.Builds[i].ID == id {
			return &c.Builds[i]
		}
	}
	return nil
}

// chain returns the builds stepping from cur to target, oldest first: each
// step is a daily applying on its parent, or a full bridging from the chain
// tip before it. Nil when no unbroken line exists — then only a full
// download reaches the target.
func (c *catalog) chain(cur, target string) []catalogEntry {
	if cur == "" || cur == target {
		return nil
	}
	var steps []catalogEntry
	for id := target; id != cur; {
		e := c.entry(id)
		if e == nil {
			return nil
		}
		prev := e.Parent
		if e.Kind == "full" {
			prev = e.BridgeFrom
		}
		if prev == "" {
			return nil
		}
		steps = append(steps, *e)
		id = prev
	}
	for i, j := 0, len(steps)-1; i < j; i, j = i+1, j-1 {
		steps[i], steps[j] = steps[j], steps[i]
	}
	return steps
}

type buildManifest struct {
	Dump  string `json:"dump"`
	Files map[string]struct {
		Size    int64  `json:"size"`
		SHA256  string `json:"sha256"`
		Content string `json:"content_hash"`
	} `json:"files"`
}

// Current reports the build this directory holds, "" for none.
func (u *Updater) Current() string {
	b, err := os.ReadFile(filepath.Join(u.Dir, "state.json"))
	if err != nil {
		return ""
	}
	var s updaterState
	if json.Unmarshal(b, &s) != nil {
		return ""
	}
	return s.Current
}

// Check reports the newest build available, and whether it is newer than
// what is held.
func (u *Updater) Check(ctx context.Context) (latest string, newer bool, err error) {
	cat, err := u.catalog(ctx)
	if err != nil {
		return "", false, err
	}
	return cat.Latest, cat.Latest > u.Current(), nil
}

func (u *Updater) catalog(ctx context.Context) (*catalog, error) {
	var cat catalog
	if err := u.getJSON(ctx, u.BaseURL+"/builds.json", &cat); err != nil {
		return nil, err
	}
	if cat.Latest == "" && cat.LatestFull != "" {
		cat.Latest = cat.LatestFull
	}
	if cat.Latest == "" {
		return nil, fmt.Errorf("filmstock: catalog lists no builds")
	}
	return &cat, nil
}

// Update brings the directory to the newest build if one is newer than what
// is held — via the patch chain when the catalog offers one, downloading the
// build whole when it does not or when the patched result fails to verify.
// It returns the path of the ready-to-open core database and the build id;
// changed is false when there was nothing to do.
func (u *Updater) Update(ctx context.Context) (corePath, build string, changed bool, err error) {
	cat, err := u.catalog(ctx)
	if err != nil {
		return "", "", false, err
	}
	latest, cur := cat.Latest, u.Current()
	if latest <= cur {
		return filepath.Join(u.Dir, cur, "filmstock.db"), cur, false, nil
	}

	if steps := cat.chain(cur, latest); steps != nil {
		if err := u.applyChain(ctx, cur, steps); err == nil {
			return u.finishBuild(ctx, latest)
		}
		// The patch road failed; say nothing and take the full road — the
		// verification below decides what is true.
		os.RemoveAll(filepath.Join(u.Dir, latest))
	}

	var man buildManifest
	if err := u.getJSON(ctx, u.BaseURL+"/"+latest+"/manifest.json", &man); err != nil {
		return "", "", false, err
	}
	dir := filepath.Join(u.Dir, latest)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", "", false, err
	}
	for _, name := range u.files() {
		want, ok := man.Files[name]
		if !ok {
			return "", "", false, fmt.Errorf("filmstock: build %s has no %s", latest, name)
		}
		dst := filepath.Join(dir, name)
		// Already present and verified — an interrupted run resumes for free.
		if sum, err := fileSHA256(dst); err == nil && sum == want.SHA256 {
			continue
		}
		if err := u.fetch(ctx, u.BaseURL+"/"+latest+"/"+name, dst, want.SHA256); err != nil {
			return "", "", false, err
		}
	}

	return u.finishBuild(ctx, latest)
}

// finishBuild rebuilds the local FTS, optionally re-verifies content, and
// flips state.json — the commit point; everything before it is invisible.
func (u *Updater) finishBuild(ctx context.Context, latest string) (string, string, bool, error) {
	var man buildManifest
	if err := u.getJSON(ctx, u.BaseURL+"/"+latest+"/manifest.json", &man); err != nil {
		return "", "", false, err
	}
	dir := filepath.Join(u.Dir, latest)
	core := filepath.Join(dir, "filmstock.db")
	h, err := sql.Open(sqldrv.Name, core)
	if err != nil {
		return "", "", false, err
	}
	if err := RebuildFTS(h); err != nil {
		h.Close()
		return "", "", false, err
	}
	if u.VerifyContent {
		got, _, err := ContentHash(h)
		if err != nil {
			h.Close()
			return "", "", false, err
		}
		if want := man.Files["filmstock.db"].Content; want != "" && got != want {
			h.Close()
			return "", "", false, fmt.Errorf(
				"filmstock: build %s content hash mismatch after rebuild: got %s want %s",
				latest, got, want)
		}
	}
	if err := h.Close(); err != nil {
		return "", "", false, err
	}
	sb, _ := json.Marshal(updaterState{Current: latest})
	if err := os.WriteFile(filepath.Join(u.Dir, "state.json"), sb, 0o644); err != nil {
		return "", "", false, err
	}
	return core, latest, true, nil
}

// applyChain copies the current build's files into the target's directory and
// applies each step's patches in order, then demands every file's content
// hash equal the target manifest's. Every failure is an error; the caller
// falls back to the whole download.
func (u *Updater) applyChain(ctx context.Context, cur string, steps []catalogEntry) error {
	target := steps[len(steps)-1].ID
	dir := filepath.Join(u.Dir, target)
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	for _, name := range u.files() {
		if err := copyLocal(filepath.Join(u.Dir, cur, name), filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	for _, step := range steps {
		var man buildManifest
		if err := u.getJSON(ctx, u.BaseURL+"/"+step.ID+"/manifest.json", &man); err != nil {
			return err
		}
		suffix := ".patch.sql.gz"
		if step.Kind == "full" {
			suffix = ".bridge.sql.gz"
		}
		for _, name := range u.files() {
			want, ok := man.Files[name+suffix]
			if !ok {
				// No patch published for this file in this step (it appeared
				// new that day): take the file whole from the step instead.
				full, ok := man.Files[name]
				if !ok {
					return fmt.Errorf("filmstock: build %s has neither %s nor its patch", step.ID, name)
				}
				if err := u.fetch(ctx, u.BaseURL+"/"+step.ID+"/"+name,
					filepath.Join(dir, name), full.SHA256); err != nil {
					return err
				}
				continue
			}
			raw, err := u.getBlob(ctx, u.BaseURL+"/"+step.ID+"/"+name+suffix, want.SHA256)
			if err != nil {
				return err
			}
			gz, err := gzip.NewReader(bytes.NewReader(raw))
			if err != nil {
				return err
			}
			sql, err := io.ReadAll(gz)
			if err != nil {
				return err
			}
			if err := applySQL(filepath.Join(dir, name), sql); err != nil {
				return fmt.Errorf("filmstock: applying %s of %s: %w", name+suffix, step.ID, err)
			}
		}
	}
	// The proof: the patched files must BE the target build, content-wise.
	var man buildManifest
	if err := u.getJSON(ctx, u.BaseURL+"/"+target+"/manifest.json", &man); err != nil {
		return err
	}
	for _, name := range u.files() {
		want := man.Files[name].Content
		if want == "" {
			continue
		}
		h, err := sql.Open(sqldrv.Name, "file:"+filepath.Join(dir, name)+"?mode=ro")
		if err != nil {
			return err
		}
		got, _, err := ContentHash(h)
		h.Close()
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("filmstock: %s after patch chain: content %s, want %s", name, got, want)
		}
	}
	return nil
}

// applySQL runs a patch against one database file, atomically: all of it or
// none of it.
func applySQL(path string, patch []byte) error {
	if len(patch) == 0 {
		return nil
	}
	h, err := sql.Open(sqldrv.Name, path)
	if err != nil {
		return err
	}
	defer h.Close()
	h.SetMaxOpenConns(1)
	if _, err := h.Exec(`BEGIN IMMEDIATE`); err != nil {
		return err
	}
	if _, err := h.Exec(string(patch)); err != nil {
		h.Exec(`ROLLBACK`)
		return err
	}
	_, err = h.Exec(`COMMIT`)
	return err
}

func copyLocal(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// UpdateAndSwap runs Update and, when a new build arrived, opens it and swaps
// it into live. The PREVIOUS *DB is returned still open: in-flight queries on
// it finish undisturbed, and the caller closes it once quiesced.
func (u *Updater) UpdateAndSwap(ctx context.Context, live *Live) (old *DB, changed bool, err error) {
	core, _, changed, err := u.Update(ctx)
	if err != nil || !changed {
		return nil, false, err
	}
	db, err := Open(core)
	if err != nil {
		return nil, false, err
	}
	return live.Swap(db), true, nil
}

// Watch checks on an interval until the context ends, swapping updates in as
// they appear. Old handles are closed after a grace period, long enough for
// any in-flight query to have finished or given up.
func (u *Updater) Watch(ctx context.Context, every time.Duration, live *Live, onErr func(error)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			old, changed, err := u.UpdateAndSwap(ctx, live)
			if err != nil && onErr != nil {
				onErr(err)
			}
			if changed && old != nil {
				go func(d *DB) {
					time.Sleep(time.Minute)
					d.Close()
				}(old)
			}
		}
	}
}

// A Live is a database handle a server reads while an updater replaces it.
type Live struct{ p atomic.Pointer[DB] }

func NewLive(db *DB) *Live {
	l := &Live{}
	l.p.Store(db)
	return l
}

// DB is the current handle. Callers use it for one operation and re-fetch
// rather than caching it across requests, so a swap takes effect immediately.
func (l *Live) DB() *DB { return l.p.Load() }

// Swap installs a new handle and returns the previous one, still open.
func (l *Live) Swap(db *DB) *DB { return l.p.Swap(db) }

func (u *Updater) local() bool { return !strings.Contains(u.BaseURL, "://") }

func (u *Updater) getJSON(ctx context.Context, url string, v any) error {
	if u.local() {
		b, err := os.ReadFile(url)
		if err != nil {
			return err
		}
		return json.Unmarshal(b, v)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	res, err := u.client().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("filmstock: GET %s: %s", url, res.Status)
	}
	return json.NewDecoder(res.Body).Decode(v)
}

// fetch streams a file to disk, hashing as it goes, and renames into place
// only when the hash matches — a torn download or a tampered byte never gets a
// real filename.
func (u *Updater) fetch(ctx context.Context, url, dst, wantSHA string) error {
	var body io.ReadCloser
	if u.local() {
		f, err := os.Open(url)
		if err != nil {
			return err
		}
		body = f
	} else {
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return err
		}
		res, err := u.client().Do(req)
		if err != nil {
			return err
		}
		if res.StatusCode != 200 {
			res.Body.Close()
			return fmt.Errorf("filmstock: GET %s: %s", url, res.Status)
		}
		body = res.Body
	}
	defer body.Close()
	tmp := dst + ".partial"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	sum := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, sum), body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != wantSHA {
		os.Remove(tmp)
		return fmt.Errorf("filmstock: %s: sha256 mismatch: got %s want %s", url, got, wantSHA)
	}
	return os.Rename(tmp, dst)
}

// getBlob fetches a small file into memory, refusing a hash mismatch.
func (u *Updater) getBlob(ctx context.Context, url, wantSHA string) ([]byte, error) {
	var body io.ReadCloser
	if u.local() {
		f, err := os.Open(url)
		if err != nil {
			return nil, err
		}
		body = f
	} else {
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return nil, err
		}
		res, err := u.client().Do(req)
		if err != nil {
			return nil, err
		}
		if res.StatusCode != 200 {
			res.Body.Close()
			return nil, fmt.Errorf("filmstock: GET %s: %s", url, res.Status)
		}
		body = res.Body
	}
	defer body.Close()
	b, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); wantSHA != "" && got != wantSHA {
		return nil, fmt.Errorf("filmstock: %s: sha256 mismatch: got %s want %s", url, got, wantSHA)
	}
	return b, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
