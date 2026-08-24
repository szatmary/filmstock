package build

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/szatmary/filmstock"
	_ "modernc.org/sqlite"
)

// A store whose fingerprint the index recorded, so the two agree.
func syncFixture(t *testing.T) (index, records string) {
	t.Helper()
	dir := t.TempDir()
	records = filepath.Join(dir, "data")
	for _, kind := range []string{"movies", "television", "people", "events"} {
		if err := os.MkdirAll(filepath.Join(records, kind), 0o755); err != nil {
			t.Fatal(err)
		}
		body := "gitdb 6 base94 9 abcd\n1 1 payload\n"
		if err := os.WriteFile(filepath.Join(records, kind, "000001.gitdb"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	index = filepath.Join(dir, "index.db")
	h, err := sql.Open("sqlite", index)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	fp, err := filmstock.StoreFingerprint(records)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Exec(`CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT);
		INSERT INTO meta VALUES('store_fingerprint', ?)`, fp); err != nil {
		t.Fatal(err)
	}
	return index, records
}

func TestIndexNeededSaysNoWhenCurrent(t *testing.T) {
	index, records := syncFixture(t)
	why, err := indexNeeded(index, records, false)
	if err != nil {
		t.Fatal(err)
	}
	if why != "" {
		t.Errorf("wanted no reindex, got %q", why)
	}
}

// The case that matters after a `git pull`: the store moved, so the index is
// behind. Detecting this is the whole reason the fingerprint is recorded, and
// before sync existed nothing acted on it.
func TestIndexNeededDetectsAChangedStore(t *testing.T) {
	index, records := syncFixture(t)
	// Append a record, exactly as a day's update would.
	f, err := os.OpenFile(filepath.Join(records, "movies", "000001.gitdb"),
		os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("2 1 another\n")
	f.Close()

	why, err := indexNeeded(index, records, false)
	if err != nil {
		t.Fatal(err)
	}
	if why == "" {
		t.Fatal("a changed store was not detected; the index would silently be stale")
	}
	t.Logf("reported: %s", why)
}

// A new shard is a changed store too, even though no existing file was touched.
func TestIndexNeededDetectsANewShard(t *testing.T) {
	index, records := syncFixture(t)
	body := "gitdb 6 base94 9 abcd\n9 1 payload\n"
	if err := os.WriteFile(filepath.Join(records, "movies", "000002.gitdb"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	why, err := indexNeeded(index, records, false)
	if err != nil {
		t.Fatal(err)
	}
	if why == "" {
		t.Fatal("a new shard did not register as a change")
	}
}

func TestIndexNeededWhenThereIsNoIndex(t *testing.T) {
	_, records := syncFixture(t)
	why, err := indexNeeded(filepath.Join(t.TempDir(), "absent.db"), records, false)
	if err != nil {
		t.Fatal(err)
	}
	if why == "" {
		t.Error("a missing index must trigger a build")
	}
}

func TestIndexNeededForce(t *testing.T) {
	index, records := syncFixture(t)
	why, err := indexNeeded(index, records, true)
	if err != nil {
		t.Fatal(err)
	}
	if why == "" {
		t.Error("-force must rebuild even when the fingerprints agree")
	}
}

// A corrupt or unreadable index must lead to a rebuild, not a hard failure:
// sync's job is to arrive at a working state.
func TestIndexNeededRebuildsOnAnUnreadableIndex(t *testing.T) {
	_, records := syncFixture(t)
	bad := filepath.Join(t.TempDir(), "broken.db")
	if err := os.WriteFile(bad, []byte("not a database"), 0o644); err != nil {
		t.Fatal(err)
	}
	why, err := indexNeeded(bad, records, false)
	if err != nil {
		t.Fatalf("an unreadable index should not be fatal: %v", err)
	}
	if why == "" {
		t.Error("an unreadable index must trigger a rebuild")
	}
}

func TestDefaultSyncDirIsNotEmpty(t *testing.T) {
	if defaultSyncDir() == "" {
		t.Error("default sync dir must never be empty")
	}
}

// An index whose build was interrupted has no meta table. CheckStore stays
// quiet about that on purpose — it cannot prove staleness without a
// fingerprint — but sync must rebuild rather than accept it.
//
// Observed for real: a reindex killed part-way left an index built from the
// previous day's store, and every later sync reported "index is current".
func TestIndexNeededRebuildsAnIndexWithNoFingerprint(t *testing.T) {
	_, records := syncFixture(t)
	half := filepath.Join(t.TempDir(), "half.db")
	h, err := sql.Open("sqlite", half)
	if err != nil {
		t.Fatal(err)
	}
	// A valid database that simply never got its meta table written.
	if _, err := h.Exec(`CREATE TABLE movies(id INTEGER PRIMARY KEY, title TEXT)`); err != nil {
		t.Fatal(err)
	}
	h.Close()

	why, err := indexNeeded(half, records, false)
	if err != nil {
		t.Fatal(err)
	}
	if why == "" {
		t.Fatal("an index with no fingerprint was accepted as current")
	}
}

// An empty fingerprint is the same problem wearing a different hat.
func TestIndexNeededRebuildsOnAnEmptyFingerprint(t *testing.T) {
	_, records := syncFixture(t)
	p := filepath.Join(t.TempDir(), "empty.db")
	h, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Exec(`CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT);
		INSERT INTO meta VALUES('store_fingerprint','')`); err != nil {
		t.Fatal(err)
	}
	h.Close()
	why, err := indexNeeded(p, records, false)
	if err != nil {
		t.Fatal(err)
	}
	if why == "" {
		t.Error("an empty fingerprint was accepted as current")
	}
}

// The maintainer's way to reclaim space is compact + squash + force-push. That
// is legitimate for a store of generated records — nothing is authored in a
// clone — but it leaves `git pull --ff-only` stuck on "Not possible to
// fast-forward". sync has to follow the remote across a rewrite.
func TestUpdateCloneFollowsARewrittenHistory(t *testing.T) {
	origin := newRepo(t)
	write(t, origin, "movies/000001.gitdb", "gitdb 6\n1 1 a\n")
	gitT(t, origin, "add", "-A")
	gitT(t, origin, "commit", "-q", "-m", "one")
	write(t, origin, "movies/000001.gitdb", "gitdb 6\n1 1 a\n2 1 b\n")
	gitT(t, origin, "add", "-A")
	gitT(t, origin, "commit", "-q", "-m", "two")

	clone := filepath.Join(t.TempDir(), "clone")
	if _, err := git(t.TempDir(), "clone", "-q", origin, clone); err != nil {
		t.Fatal(err)
	}

	// The maintainer squashes and force-pushes.
	gitT(t, origin, "checkout", "-q", "--orphan", "squashed")
	gitT(t, origin, "add", "-A")
	gitT(t, origin, "commit", "-q", "-m", "compacted and squashed")
	gitT(t, origin, "branch", "-q", "-D", "main")
	gitT(t, origin, "branch", "-q", "-m", "main")

	if err := updateClone(clone); err != nil {
		t.Fatalf("sync could not follow the rewrite: %v", err)
	}
	got := strings.TrimSpace(gitT(t, clone, "log", "-1", "--pretty=%s"))
	if got != "compacted and squashed" {
		t.Errorf("clone is at %q, want the rewritten history", got)
	}
	if st := gitT(t, clone, "status", "--porcelain"); strings.TrimSpace(st) != "" {
		t.Errorf("clone left dirty after reset:\n%s", st)
	}
}

// The ordinary case must stay a plain fast-forward.
func TestUpdateCloneFastForwardsNormally(t *testing.T) {
	origin := newRepo(t)
	write(t, origin, "f", "one")
	gitT(t, origin, "add", "-A")
	gitT(t, origin, "commit", "-q", "-m", "one")
	clone := filepath.Join(t.TempDir(), "clone")
	if _, err := git(t.TempDir(), "clone", "-q", origin, clone); err != nil {
		t.Fatal(err)
	}
	write(t, origin, "f", "two")
	gitT(t, origin, "add", "-A")
	gitT(t, origin, "commit", "-q", "-m", "two")

	if err := updateClone(clone); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(gitT(t, clone, "log", "-1", "--pretty=%s")); got != "two" {
		t.Errorf("clone at %q, want two", got)
	}
}
