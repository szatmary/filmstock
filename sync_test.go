package filmstock

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func originRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	gitIn(t, dir, "config", "user.name", "t")
	gitIn(t, dir, "config", "user.email", "t@e.com")
	writeStore(t, dir, "1 1 a\n")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "one")
	return dir
}

func writeStore(t *testing.T, dir, body string) {
	t.Helper()
	for _, kind := range []string{"movies", "television", "people", "events"} {
		if err := os.MkdirAll(filepath.Join(dir, kind), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, kind, "000001.gitdb"),
			[]byte("gitdb 6 base94 9 abcd\n"+body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSyncStoreClonesWhenAbsent(t *testing.T) {
	origin := originRepo(t)
	dst := filepath.Join(t.TempDir(), "data")
	fp, changed, err := syncStore(context.Background(), origin, dst, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("a fresh clone should report changed")
	}
	if fp == "" {
		t.Error("no fingerprint returned")
	}
	if _, err := os.Stat(filepath.Join(dst, "movies", "000001.gitdb")); err != nil {
		t.Errorf("store not checked out: %v", err)
	}
}

func TestSyncStoreIsIdempotent(t *testing.T) {
	origin := originRepo(t)
	dst := filepath.Join(t.TempDir(), "data")
	fp1, _, err := syncStore(context.Background(), origin, dst, nil)
	if err != nil {
		t.Fatal(err)
	}
	fp2, changed, err := syncStore(context.Background(), origin, dst, nil)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("second sync reported a change with nothing to fetch")
	}
	if fp1 != fp2 {
		t.Errorf("fingerprint moved without the store moving: %s -> %s", fp1[:12], fp2[:12])
	}
}

// Reclaiming space in a git-hosted store means compact + squash + force-push.
// That is legitimate for generated records — nothing is authored in a clone —
// but it leaves a plain fast-forward stuck on "Not possible to fast-forward".
// Consumers have to follow the remote across it.
func TestSyncStoreFollowsARewrittenHistory(t *testing.T) {
	origin := originRepo(t)
	dst := filepath.Join(t.TempDir(), "data")
	if _, _, err := syncStore(context.Background(), origin, dst, nil); err != nil {
		t.Fatal(err)
	}

	// The maintainer compacts, squashes and force-pushes.
	writeStore(t, origin, "1 2 rewritten\n")
	gitIn(t, origin, "checkout", "-q", "--orphan", "squashed")
	gitIn(t, origin, "add", "-A")
	gitIn(t, origin, "commit", "-q", "-m", "compacted and squashed")
	gitIn(t, origin, "branch", "-q", "-D", "main")
	gitIn(t, origin, "branch", "-q", "-m", "main")

	_, changed, err := syncStore(context.Background(), origin, dst, nil)
	if err != nil {
		t.Fatalf("could not follow the rewrite: %v", err)
	}
	if !changed {
		t.Error("a rewritten history should report a change")
	}
	body, err := os.ReadFile(filepath.Join(dst, "movies", "000001.gitdb"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "rewritten") {
		t.Errorf("clone did not follow the rewrite; still holds:\n%s", body)
	}
}

// A directory with unrelated contents must not be clobbered.
func TestSyncStoreRefusesANonCheckout(t *testing.T) {
	origin := originRepo(t)
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, "important.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := syncStore(context.Background(), origin, dst, nil); err == nil {
		t.Error("syncing into a non-empty, non-checkout directory should fail")
	}
	if _, err := os.Stat(filepath.Join(dst, "important.txt")); err != nil {
		t.Error("existing contents were removed")
	}
}

func TestSyncStoreReportsProgress(t *testing.T) {
	origin := originRepo(t)
	dst := filepath.Join(t.TempDir(), "data")
	var lines []string
	if _, _, err := syncStore(context.Background(), origin, dst,
		func(s string) { lines = append(lines, s) }); err != nil {
		t.Fatal(err)
	}
	if len(lines) == 0 {
		t.Error("no progress reported for a clone of unknown duration")
	}
}
