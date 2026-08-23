package build

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultCommitMessageNamesTheDump(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"/d/enwiki-20260802-pages-meta-hist-incr.xml.bz2", "Apply 20260802 adds-changes"},
		{"enwiki-20260801-pages-meta-hist-incr.xml.bz2", "Apply 20260801 adds-changes"},
	} {
		if got := defaultCommitMessage(c.in); got != c.want {
			t.Errorf("defaultCommitMessage(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The .gitdb/.idx split is the whole point of the report: data files are
// appended to and index files are rewritten, and conflating them hides which of
// the two is actually costing repository space.
func TestDiffStatsSplitsDataFromIndex(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "movies/000001.gitdb", "a\nb\n")
	write(t, dir, "movies/000001.idx", "1\n2\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "base")

	// One appended record line, one rewritten index line.
	write(t, dir, "movies/000001.gitdb", "a\nb\nc\n")
	write(t, dir, "movies/000001.idx", "1\n9\n")

	s, err := diffStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.files != 2 {
		t.Errorf("files = %d, want 2", s.files)
	}
	if s.dataAdded != 1 || s.dataDel != 0 {
		t.Errorf(".gitdb = +%d -%d, want +1 -0", s.dataAdded, s.dataDel)
	}
	if s.indexAdded != 1 || s.indexDel != 1 {
		t.Errorf(".idx = +%d -%d, want +1 -1", s.indexAdded, s.indexDel)
	}
}

// A clean tree must not produce an empty commit: repeating an already-applied
// day would otherwise litter history with commits that change nothing.
func TestReportAndCommitSkipsAnUnchangedTree(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "movies/000001.gitdb", "a\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "base")
	before := strings.TrimSpace(gitT(t, dir, "rev-list", "--count", "HEAD"))

	var buf bytes.Buffer
	if err := reportAndCommit(&buf, dir, "should not happen", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "nothing changed") {
		t.Errorf("expected 'nothing changed', got: %s", buf.String())
	}
	if after := strings.TrimSpace(gitT(t, dir, "rev-list", "--count", "HEAD")); after != before {
		t.Errorf("commit count went %s -> %s; an empty commit was made", before, after)
	}
}

func TestReportAndCommitCommitsWhenAsked(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "movies/000001.gitdb", "a\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "base")
	write(t, dir, "movies/000001.gitdb", "a\nb\n")

	var buf bytes.Buffer
	if err := reportAndCommit(&buf, dir, "Apply 20260802 adds-changes", true); err != nil {
		t.Fatal(err)
	}
	if got := gitT(t, dir, "log", "-1", "--pretty=%s"); !strings.Contains(got, "Apply 20260802") {
		t.Errorf("HEAD subject = %q", got)
	}
	if !strings.Contains(buf.String(), "committed") {
		t.Errorf("expected a 'committed' line, got: %s", buf.String())
	}
}

// Without -commit the change must be left in the working tree: measuring a diff
// and recording it are separate decisions.
func TestReportAndCommitLeavesTreeDirtyWithoutFlag(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "movies/000001.gitdb", "a\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "base")
	write(t, dir, "movies/000001.gitdb", "a\nb\n")

	var buf bytes.Buffer
	if err := reportAndCommit(&buf, dir, "unused", false); err != nil {
		t.Fatal(err)
	}
	if st := gitT(t, dir, "status", "--porcelain"); strings.TrimSpace(st) == "" {
		t.Error("working tree was committed despite commit=false")
	}
}

func TestIsGitRepoRejectsPlainDirectory(t *testing.T) {
	dir := t.TempDir()
	if isGitRepo(dir) {
		t.Error("a plain directory reported as a git work tree")
	}
	var buf bytes.Buffer
	if err := reportAndCommit(&buf, dir, "m", true); err == nil {
		t.Error("-commit on a non-repository should be an error, not a silent no-op")
	}
}

func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	gitT(t, dir, "config", "user.name", "test")
	gitT(t, dir, "config", "user.email", "test@example.com")
	return dir
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A new .gitdb shard is untracked, and `git diff` ignores untracked files
// entirely. Reporting only tracked changes understated a real day by 160 lines —
// the exact days that roll over a shard are the ones that grew the store most.
func TestDiffStatsCountsUntrackedShards(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "television/000039.gitdb", "a\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "base")

	write(t, dir, "television/000039.gitdb", "a\nb\n")    // tracked: +1
	write(t, dir, "television/000040.gitdb", "x\ny\nz\n") // untracked: +3

	s, err := diffStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.dataAdded != 4 {
		t.Errorf(".gitdb added = %d, want 4 (1 tracked + 3 in the new shard)", s.dataAdded)
	}
	if s.files != 2 {
		t.Errorf("files = %d, want 2", s.files)
	}
}

func TestCountLinesHandlesMissingFinalNewline(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		body string
		want int
	}{
		{"a\nb\nc\n", 3},
		{"a\nb\nc", 3},
		{"", 0},
		{"a", 1},
	} {
		p := filepath.Join(dir, "f")
		if err := os.WriteFile(p, []byte(c.body), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := countLines(p)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("countLines(%q) = %d, want %d", c.body, got, c.want)
		}
	}
}

// An ignored file must not be counted: .gitignore'd scratch in the store
// directory is not part of what a clone pays for.
func TestDiffStatsIgnoresIgnoredFiles(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, ".gitignore", "*.tmp\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "base")
	write(t, dir, "scratch.tmp", "1\n2\n3\n")

	s, err := diffStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.files != 0 {
		t.Errorf("files = %d, want 0 (the only new file is ignored)", s.files)
	}
}
