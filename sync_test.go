package filmstock

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// commitFile writes rel inside the upstream fixture repo's worktree and
// commits it, returning nothing; the test reads state through SyncStore.
func commitFile(t *testing.T, upstream string, rel string, contents []byte) {
	t.Helper()
	repo, err := git.PlainOpen(upstream)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(upstream, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, contents, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add(rel); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "fixture", Email: "fixture@test", When: time.Now()}
	if _, err := wt.Commit("Append "+rel, &git.CommitOptions{Author: sig}); err != nil {
		t.Fatal(err)
	}
}

// newUpstream creates a local fixture data repo holding one fake store file.
func newUpstream(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := git.PlainInit(dir, false); err != nil {
		t.Fatal(err)
	}
	commitFile(t, dir, filepath.Join(KindMovie, "000001.gitdb"), []byte("record-one\n"))
	return dir
}

func TestSyncStoreClonesAndFingerprintsFreshCheckout(t *testing.T) {
	upstream := newUpstream(t)
	dir := filepath.Join(t.TempDir(), "filmstock-data")

	var lines []string
	fp, changed, err := syncStore(context.Background(), upstream, dir, func(s string) { lines = append(lines, s) })
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("fresh clone must report changed=true")
	}
	want, err := StoreFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fp != want {
		t.Errorf("fingerprint = %s, StoreFingerprint(dir) = %s", fp, want)
	}
	if len(lines) == 0 {
		t.Error("expected at least one progress line")
	}
	if _, err := os.Stat(filepath.Join(dir, KindMovie, "000001.gitdb")); err != nil {
		t.Errorf("cloned store file missing: %v", err)
	}
}

func TestSyncStoreUnchangedReportsFalseAndSameFingerprint(t *testing.T) {
	upstream := newUpstream(t)
	dir := filepath.Join(t.TempDir(), "filmstock-data")
	fp1, _, err := syncStore(context.Background(), upstream, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	fp2, changed, err := syncStore(context.Background(), upstream, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("no upstream commits: changed must be false")
	}
	if fp1 != fp2 {
		t.Errorf("fingerprint moved without changes: %s -> %s", fp1, fp2)
	}
}

func TestSyncStoreUpdatesAndHardResets(t *testing.T) {
	upstream := newUpstream(t)
	dir := filepath.Join(t.TempDir(), "filmstock-data")
	fp1, _, err := syncStore(context.Background(), upstream, dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Dirty the local checkout the way a crashed import might, then grow
	// the upstream store: sync must land exactly on the upstream state.
	local := filepath.Join(dir, KindMovie, "000001.gitdb")
	if err := os.WriteFile(local, []byte("locally-corrupted"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitFile(t, upstream, filepath.Join(KindMovie, "000001.gitdb"), []byte("record-one\nrecord-two\n"))

	fp2, changed, err := syncStore(context.Background(), upstream, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("upstream grew: changed must be true")
	}
	if fp1 == fp2 {
		t.Error("fingerprint must move when a store file's size changes")
	}
	got, err := os.ReadFile(local)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "record-one\nrecord-two\n" {
		t.Errorf("hard reset did not restore upstream contents: %q", got)
	}
}

func TestSyncStoreRefusesDirThatIsNotACheckout(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := syncStore(context.Background(), "ignored", dir, nil); err == nil {
		t.Fatal("a non-empty non-checkout dir must be an error, never guessed at or deleted")
	}
}

func TestSyncStoreExportedUsesCanonicalURL(t *testing.T) {
	if DataRepoURL != "https://github.com/szatmary/filmstock-data.git" {
		t.Errorf("DataRepoURL = %q", DataRepoURL)
	}
}
