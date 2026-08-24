package build

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/szatmary/filmstock"
)

// Getting from nothing to a searchable database took two commands and knowing
// the data repository's URL: clone it, then discover that `filmstock index`
// exists and run it. Worse, after a `git pull` there was no signal that the
// index was now behind the store — the fingerprint to detect it was recorded and
// compared, but nothing acted on it.
//
// sync is those steps as one idempotent command: clone or pull, then reindex
// only when the store actually moved.

const defaultDataRepo = "https://github.com/szatmary/filmstock-data.git"

// CmdSync clones or updates the record store and rebuilds the index if stale.
func CmdSync(args []string) {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	dir := fs.String("dir", defaultSyncDir(), "where the store and index live")
	repo := fs.String("repo", defaultDataRepo, "record store repository")
	force := fs.Bool("force", false, "reindex even if the store has not changed")
	pull := fs.Bool("pull", true, "fetch changes for an existing clone")
	fs.Parse(args)

	records := filepath.Join(*dir, "data")
	index := filepath.Join(*dir, "index.db")

	if err := os.MkdirAll(*dir, 0o777); err != nil {
		fatal(err)
	}

	start := time.Now()
	switch {
	case !isGitRepo(records):
		if _, err := os.Stat(records); err == nil {
			fatal(fmt.Errorf("%s exists but is not a git clone; move it aside or use -dir", records))
		}
		fmt.Fprintf(os.Stderr, "cloning %s -> %s\n", *repo, records)
		if err := runGit(*dir, "clone", "--progress", *repo, records); err != nil {
			fatal(err)
		}
	case *pull:
		fmt.Fprintf(os.Stderr, "updating %s\n", records)
		before, _ := gitRev(records)
		if err := updateClone(records); err != nil {
			fatal(err)
		}
		after, _ := gitRev(records)
		if before == after {
			fmt.Fprintf(os.Stderr, "  already at %s\n", short(after))
		} else {
			fmt.Fprintf(os.Stderr, "  %s -> %s\n", short(before), short(after))
		}
	default:
		fmt.Fprintln(os.Stderr, "-pull=false; using the clone as it is")
	}

	// Reindex only when the store has actually moved. The fingerprint is what
	// the index recorded about the store it was built from, so this is a real
	// comparison rather than a timestamp guess.
	why, err := indexNeeded(index, records, *force)
	if err != nil {
		fatal(err)
	}
	if why == "" {
		fmt.Fprintf(os.Stderr, "index is current; nothing to do (%.1fs)\n", time.Since(start).Seconds())
		fmt.Fprintf(os.Stderr, "\n  records %s\n  index   %s\n", records, index)
		return
	}
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

// updateClone brings the clone to whatever the remote now says, including when
// the remote's history was rewritten.
//
// A record store is derived data: nothing here is authored locally, so there is
// nothing a reset could destroy. That matters because the maintainer's way to
// reclaim space is to compact and squash and force-push — which is a perfectly
// good thing to do to a repository of generated records, and would otherwise
// leave every existing clone stuck on "Not possible to fast-forward, aborting".
//
// A fast-forward is still tried first, so the ordinary case stays ordinary and
// only a genuine divergence takes the heavier path.
func updateClone(records string) error {
	if err := runGit(records, "pull", "--ff-only"); err == nil {
		return nil
	}
	fmt.Fprintln(os.Stderr, "  remote history was rewritten; resetting to it")
	if err := runGit(records, "fetch", "--prune", "origin"); err != nil {
		return err
	}
	branch, err := git(records, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	return runGit(records, "reset", "--hard", "origin/"+strings.TrimSpace(branch))
}

// indexNeeded reports why the index must be rebuilt, or "" if it is current.
func indexNeeded(index, records string, force bool) (string, error) {
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

	db := filmstock.FromSQL(h, nil)
	switch err := db.CheckStore(records); {
	case err == nil:
		return "", nil
	case errors.Is(err, filmstock.ErrStaleIndex):
		return "the store changed since the index was built", nil
	default:
		return "the index could not be checked", nil //nolint:nilerr
	}
}

// defaultSyncDir follows the XDG cache convention, falling back to the working
// directory when there is no home to speak of.
func defaultSyncDir() string {
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "filmstock")
	}
	return "filmstock-cache"
}

// runGit streams git's output through, because a 429 MB clone with no progress
// looks indistinguishable from a hang.
func runGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func gitRev(dir string) (string, error) {
	out, err := git(dir, "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

func short(rev string) string {
	if len(rev) > 8 {
		return rev[:8]
	}
	return rev
}
