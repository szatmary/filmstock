package filmstock

import (
	"context"
	"errors"
	"fmt"
	"os"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// DataRepoURL is the canonical filmstock-data repository. The library
// hardcodes it: consumers say where the checkout lives, never where the
// data comes from.
const DataRepoURL = "https://github.com/szatmary/filmstock-data.git"

// SyncStore clones the canonical data repo into dir if dir does not hold
// a checkout, else fetches and hard-resets to the remote head. It returns
// the store fingerprint of the resulting tree (StoreFingerprint) and
// whether the tree changed. Progress, if non-nil, receives coarse
// human-readable sync progress lines.
func SyncStore(ctx context.Context, dir string, progress func(string)) (string, bool, error) {
	return syncStore(ctx, DataRepoURL, dir, progress)
}

func syncStore(ctx context.Context, url, dir string, progress func(string)) (string, bool, error) {
	say := func(s string) {
		if progress != nil {
			progress(s)
		}
	}

	repo, err := git.PlainOpen(dir)
	switch {
	case errors.Is(err, git.ErrRepositoryNotExists):
		if entries, rerr := os.ReadDir(dir); rerr == nil && len(entries) > 0 {
			return "", false, fmt.Errorf("filmstock: %s exists but is not a filmstock-data checkout", dir)
		}
		say("cloning " + url)
		if _, err := git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{URL: url}); err != nil {
			return "", false, fmt.Errorf("filmstock: clone %s: %w", url, err)
		}
		fp, err := StoreFingerprint(dir)
		return fp, true, err
	case err != nil:
		return "", false, fmt.Errorf("filmstock: open checkout %s: %w", dir, err)
	}

	say("fetching updates")
	if err := repo.FetchContext(ctx, &git.FetchOptions{}); err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return "", false, fmt.Errorf("filmstock: fetch: %w", err)
	}
	head, err := repo.Head()
	if err != nil {
		return "", false, fmt.Errorf("filmstock: head: %w", err)
	}
	remote, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", head.Name().Short()), true)
	if err != nil {
		return "", false, fmt.Errorf("filmstock: remote head: %w", err)
	}

	changed := remote.Hash() != head.Hash()
	if changed {
		say("resetting to " + remote.Hash().String()[:12])
		wt, err := repo.Worktree()
		if err != nil {
			return "", false, err
		}
		if err := wt.Reset(&git.ResetOptions{Mode: git.HardReset, Commit: remote.Hash()}); err != nil {
			return "", false, fmt.Errorf("filmstock: reset: %w", err)
		}
	} else {
		say("up to date")
	}
	fp, err := StoreFingerprint(dir)
	return fp, changed, err
}
