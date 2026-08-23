package gitdb

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestCompactDropsSupersededAndTombstones(t *testing.T) {
	dir := t.TempDir()
	db := open(t, dir)
	for i := range 20 {
		put(t, db, fmt.Sprint(i), incompressible(i, 120))
	}
	for i := range 20 { // every record superseded once
		put(t, db, fmt.Sprint(i), incompressible(i+1000, 120))
	}
	if err := db.Delete("3"); err != nil {
		t.Fatal(err)
	}
	beforeBytes := storeBytes(t, db)

	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	afterBytes := storeBytes(t, db)
	if afterBytes >= beforeBytes {
		t.Errorf("compaction did not reclaim space: %d -> %d", beforeBytes, afterBytes)
	}
	if db.Len() != 19 {
		t.Errorf("Len = %d, want 19", db.Len())
	}
	if _, err := db.Get("3"); err != ErrNotFound {
		t.Errorf("deleted key survived compaction: %v", err)
	}
	// Values must survive, read through the compacted offsets.
	for i := range 20 {
		if i == 3 {
			continue
		}
		want := incompressible(i+1000, 120)
		if got := get(t, db, fmt.Sprint(i)); got != want {
			t.Fatalf("key %d = %q, want %q", i, got, want)
		}
	}
	// And through a fresh scan of what is on disk.
	db2 := open(t, dir)
	if db2.Len() != 19 {
		t.Errorf("after reopen Len = %d, want 19", db2.Len())
	}
	if got := get(t, db2, "7"); got != incompressible(7+1000, 120) {
		t.Errorf("key 7 wrong after reopen")
	}
}

func TestCompactLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	db := open(t, dir, WithMaxFileSize(4096))
	for i := range 60 {
		put(t, db, fmt.Sprint(i), incompressible(i, 100))
	}
	for i := range 30 {
		put(t, db, fmt.Sprint(i), incompressible(i+500, 100))
	}
	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") || e.Name() == compactMarker {
			t.Errorf("left behind %q", e.Name())
		}
	}
}

func storeBytes(t *testing.T, db *DB) int64 {
	t.Helper()
	files, err := db.recordFiles()
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	for _, i := range files {
		fi, err := os.Stat(db.path(i))
		if err != nil {
			t.Fatal(err)
		}
		n += fi.Size()
	}
	return n
}

// The reason this format exists: a day's worth of updates must show up in git as
// appended lines and nothing else. If any existing line is rewritten, the diff
// grows with the size of the store instead of the size of the change — which is
// exactly what the .idx files used to do.
func TestGitDiffOfAnUpdateIsPureAppend(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	run("init", "-q", "-b", "main")
	run("config", "user.name", "t")
	run("config", "user.email", "t@e.com")

	db := open(t, dir)
	for i := range 200 {
		put(t, db, fmt.Sprint(i), incompressible(i, 150))
	}
	run("add", "-A")
	run("commit", "-q", "-m", "base")

	// A day's changes: update 5 records, insert 1.
	for _, i := range []int{3, 40, 77, 120, 199} {
		put(t, db, fmt.Sprint(i), incompressible(i+9000, 150))
	}
	put(t, db, "10000", incompressible(1, 150))

	stat := run("diff", "--numstat")
	var added, deleted int
	for _, line := range strings.Split(strings.TrimSpace(stat), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		var a, d int
		fmt.Sscanf(f[0], "%d", &a)
		fmt.Sscanf(f[1], "%d", &d)
		added += a
		deleted += d
	}
	if deleted != 0 {
		t.Errorf("diff deleted %d lines; an update must not rewrite existing lines\n%s", deleted, stat)
	}
	if added != 6 {
		t.Errorf("diff added %d lines, want 6 (5 updates + 1 insert)\n%s", added, stat)
	}
}
