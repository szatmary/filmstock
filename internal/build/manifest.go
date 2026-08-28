package build

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/szatmary/filmstock"
	"github.com/szatmary/filmstock/internal/sqldrv"
)

// The release manifest: what a consumer fetches first.
//
// Distribution is plain HTTPS from object storage — no git, no torrents, no
// content addressing — so the manifest carries the two things that layer no
// longer provides: WHICH files make up a release, and proof the bytes arrived
// intact. SHA-256 per file does the job an infohash would have.
//
// Layout on the bucket, by convention:
//
//	/filmstock/<dump>/filmstock.db            immutable once written
//	/filmstock/<dump>/filmstock-text.db
//	/filmstock/<dump>/filmstock-vectors.db
//	/filmstock/<dump>/manifest.json
//	/filmstock/latest.json                    a COPY of the newest manifest
//
// Versioned paths are immutable because a URL that quietly changes its bytes
// breaks every consumer that cached, resumed, or half-fetched it; "latest" is
// the one mutable pointer, and it is a copy rather than a redirect so a single
// GET answers both "what is current" and "how do I verify it".
type manifest struct {
	Dump         string                  `json:"dump"`      // e.g. 20260801
	Generated    string                  `json:"generated"` // RFC 3339, UTC
	ContentHashV int                     `json:"content_hash_version,omitempty"`
	Files        map[string]manifestFile `json:"files"`
}

type manifestFile struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"` // of the file bytes: verifies the download
	// Content verifies MEANING: rows in canonical order, storage-independent.
	// A consumer who applied patches matches this while matching neither the
	// size nor the file hash, and that is the point.
	Content       string            `json:"content_hash,omitempty"`
	ContentTables map[string]string `json:"content_tables,omitempty"`
}

// CmdManifest hashes a release directory into its manifest.
func CmdManifest(args []string) {
	fs := flag.NewFlagSet("manifest", flag.ExitOnError)
	dir := fs.String("dir", "bucket", "release directory holding the artifacts")
	dump := fs.String("dump", "", "dump date the release was built from (YYYYMMDD)")
	out := fs.String("out", "", "where to write (default <dir>/manifest.json)")
	fs.Parse(args)
	if *dump == "" {
		fatal(fmt.Errorf("manifest needs -dump YYYYMMDD: a release that does not " +
			"say what it was built from cannot be reasoned about later"))
	}
	if *out == "" {
		*out = filepath.Join(*dir, "manifest.json")
	}

	entries, err := os.ReadDir(*dir)
	if err != nil {
		fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || e.Name() == "manifest.json" || e.Name() == "latest.json" {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if len(names) == 0 {
		fatal(fmt.Errorf("manifest: nothing to describe in %s", *dir))
	}
	files := map[string]string{}
	for _, name := range names {
		files[name] = filepath.Join(*dir, name)
	}
	if err := writeManifest(*out, *dump, files); err != nil {
		fatal(err)
	}
}

// writeManifest describes the named files — wherever each lives: a daily's
// databases sit in the un-hosted work directory while its patches sit in the
// hosted one, and one manifest speaks for both.
func writeManifest(out, dump string, files map[string]string) error {
	m := manifest{
		Dump:         dump,
		Generated:    time.Now().UTC().Format(time.RFC3339),
		ContentHashV: filmstock.ContentHashVersion,
		Files:        map[string]manifestFile{},
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := files[name]
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		h := sha256.New()
		size, err := io.Copy(h, f)
		f.Close()
		if err != nil {
			return err
		}
		mf := manifestFile{Size: size, SHA256: hex.EncodeToString(h.Sum(nil))}
		if strings.HasSuffix(name, ".db") {
			ch, tables, err := contentHashOf(path)
			if err != nil {
				return fmt.Errorf("content hash of %s: %w", name, err)
			}
			mf.Content, mf.ContentTables = ch, tables
		}
		m.Files[name] = mf
		fmt.Fprintf(os.Stderr, "  %-28s %5d MB  file %s  content %s\n", name, size/1048576,
			mf.SHA256[:12]+"…", short(mf.Content))
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "  -> %s\n", out)
	return nil
}

func contentHashOf(path string) (string, map[string]string, error) {
	h, err := sql.Open(sqldrv.Name, "file:"+path+"?mode=ro")
	if err != nil {
		return "", nil, err
	}
	defer h.Close()
	return filmstock.ContentHash(h)
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	if s == "" {
		return "—"
	}
	return s
}
