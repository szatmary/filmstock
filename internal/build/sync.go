package build

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/szatmary/filmstock"
)

// The command that gets a consumer from nothing to a searchable database.
//
// Moving the store around is filmstock.SyncStore's job, not this one's: the
// library clones and updates through go-git so an importer needs no git binary,
// and duplicating that here with shell-outs would mean two implementations of
// "follow the remote, including when its history was rewritten" that could drift
// apart. This adds what SyncStore deliberately leaves out — rebuilding the index
// when the store has moved, and fetching the embedding vectors.
//
// Maintainer-side code in this package still shells out to git (see
// gitreport.go), which is the right split: committing and diffing are things a
// maintainer does with a real git installed, while a consumer should not need
// one at all.

// The vectors are published as a release asset rather than committed: they are
// 168 MB, regenerable, and a new one lands with every corpus, so in git history
// they would accumulate a fresh blob each time.
//
// "latest" rather than a pinned tag, because a pinned one goes stale the moment
// a new corpus is published and nobody would notice until the coverage dropped.
const defaultVectorsURL = "https://github.com/szatmary/filmstock-data/releases/latest/download/vectors.bin"

// CmdSync clones or updates the record store and rebuilds the index if stale.
func CmdSync(args []string) {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	dir := fs.String("dir", defaultSyncDir(), "where the store and index live")
	force := fs.Bool("force", false, "reindex even if the store has not changed")
	vecURL := fs.String("vectors", defaultVectorsURL, "embedding vectors to fetch (empty to skip)")
	fs.Parse(args)

	records := filepath.Join(*dir, "data")
	index := filepath.Join(*dir, "index.db")

	if err := os.MkdirAll(*dir, 0o777); err != nil {
		fatal(err)
	}

	start := time.Now()
	fp, changed, err := filmstock.SyncStore(context.Background(), records,
		func(line string) { fmt.Fprintf(os.Stderr, "  %s\n", line) })
	if err != nil {
		fatal(err)
	}
	if !changed {
		fmt.Fprintln(os.Stderr, "  store unchanged")
	}

	// Reindex only when the store has actually moved. The fingerprint is what
	// the index recorded about the store it was built from, so this is a real
	// comparison rather than a timestamp guess.
	why, err := indexNeeded(index, fp, *force)
	if err != nil {
		fatal(err)
	}
	if why == "" {
		fetchVectors(*vecURL, filepath.Join(*dir, "vectors.bin"))
		fmt.Fprintf(os.Stderr, "index is current; nothing to do (%.1fs)\n", time.Since(start).Seconds())
		fmt.Fprintf(os.Stderr, "\n  records %s\n  index   %s\n", records, index)
		return
	}
	fetchVectors(*vecURL, filepath.Join(*dir, "vectors.bin"))

	fmt.Fprintf(os.Stderr, "reindexing: %s\n", why)
	// Build beside the real index and rename into place. A killed run then
	// leaves the previous index untouched instead of a truncated one that the
	// next run has to recognise as broken.
	tmp := index + ".building"
	os.Remove(tmp)
	CIndexRecords([]string{"-records", records, "-db", tmp})
	if err := os.Rename(tmp, index); err != nil {
		fatal(fmt.Errorf("installing the new index: %w", err))
	}
	for _, sfx := range []string{"-wal", "-shm", "-journal"} {
		os.Rename(tmp+sfx, index+sfx)
	}
	fmt.Fprintf(os.Stderr, "\nready in %.1fs\n  records %s\n  index   %s\n",
		time.Since(start).Seconds(), records, index)
}

// fetchVectors downloads the embedding vectors if they are not already there.
//
// Optional on purpose: everything except similarity and exploration works
// without them, so a failure here is reported and moved past rather than
// failing a sync that otherwise succeeded.
func fetchVectors(url, dest string) {
	if url == "" {
		return
	}
	if fi, err := os.Stat(dest); err == nil && fi.Size() > 0 {
		fmt.Fprintf(os.Stderr, "vectors: have %s (%.0f MB)\n", dest, float64(fi.Size())/(1<<20))
		return
	}
	fmt.Fprintf(os.Stderr, "vectors: fetching %s\n", url)
	resp, err := httpGet(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vectors: %v (similarity will be unavailable)\n", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "vectors: %s: %s (similarity will be unavailable)\n", url, resp.Status)
		return
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vectors: %v\n", err)
		return
	}
	n, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		fmt.Fprintf(os.Stderr, "vectors: %v\n", err)
		return
	}
	if resp.ContentLength > 0 && n != resp.ContentLength {
		os.Remove(tmp)
		fmt.Fprintf(os.Stderr, "vectors: short read (%d of %d bytes)\n", n, resp.ContentLength)
		return
	}
	if err := os.Rename(tmp, dest); err != nil {
		fmt.Fprintf(os.Stderr, "vectors: %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "vectors: %.0f MB\n", float64(n)/(1<<20))
}

// indexNeeded reports why the index must be rebuilt, or "" if it is current.
func indexNeeded(index, storeFingerprint string, force bool) (string, error) {
	if force {
		return "-force given", nil
	}
	if _, err := os.Stat(index); os.IsNotExist(err) {
		return "no index yet", nil
	} else if err != nil {
		return "", err
	}
	h, err := sql.Open("sqlite", index)
	if err != nil {
		return "the index could not be opened", nil //nolint:nilerr // rebuild rather than fail
	}
	defer h.Close()

	// An index with no recorded fingerprint must be rebuilt, not accepted.
	//
	// CheckStore deliberately stays quiet about one of these: it cannot prove
	// staleness without a fingerprint, and a library should not claim more than
	// it knows. But sync's job is to arrive at a working index, and the usual
	// way to get an index with no meta table is an interrupted build — which is
	// exactly the case that must not be treated as current. Observed for real: a
	// reindex killed part-way left an index built from the previous day's store,
	// and sync then reported "index is current" on every subsequent run.
	var fp string
	switch err := h.QueryRow(
		`SELECT value FROM meta WHERE key = 'store_fingerprint'`).Scan(&fp); {
	case err != nil:
		return "the index records no store fingerprint (interrupted build?)", nil //nolint:nilerr
	case fp == "":
		return "the index records an empty store fingerprint", nil
	}

	if fp != storeFingerprint {
		return "the store changed since the index was built", nil
	}
	return "", nil
}

// defaultSyncDir follows the XDG cache convention, falling back to the working
// directory when there is no home to speak of.
func defaultSyncDir() string {
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "filmstock")
	}
	return "filmstock-cache"
}
