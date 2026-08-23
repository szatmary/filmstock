package build

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Applying a day's changes and recording it in git is one operation, not two.
// Doing it by hand — or from a shell script driving the binary — kept producing
// numbers that could not be compared: a diff measured after an unrelated commit,
// a .git size read while `git gc` was still rewriting the object store, a run
// whose timing was polluted by whatever else happened to be touching the disk.
// Keeping it in the tool makes the measurement reproducible and the workflow one
// command.

// gitStats is the shape of a record-store diff. Everything should now land in
// the .gitdb tally as pure appends; the .idx tally is kept so that an index file
// reappearing in a store shows up in the report rather than passing unnoticed.
type gitStats struct {
	files                int
	dataAdded, dataDel   int
	indexAdded, indexDel int
}

func (s gitStats) empty() bool { return s.files == 0 }

// git runs a git command in dir and returns stdout. Errors carry stderr, which
// is where git says anything useful.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var errb strings.Builder
	cmd.Stderr = &errb
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return string(out), nil
}

// isGitRepo reports whether dir is inside a work tree. A record store that is
// not in git is a perfectly normal thing to update; it just cannot be committed.
func isGitRepo(dir string) bool {
	out, err := git(dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true"
}

// diffStats reads `git diff --numstat` over the working tree, PLUS the untracked
// files, which `git diff` does not report at all.
//
// That omission is not academic here: a store rolls over to a new .gitdb shard
// whenever the current one reaches the file cap, and the whole new shard is
// untracked. The first run of this code reported 383 appended lines for a day
// that actually appended 543 — the missing 160 were a fresh television shard.
// Counting only what git already tracks understates exactly the days that grew
// the store the most.
//
// Untracked files are counted without staging anything: `git add -N` would make
// them visible to `git diff` but mutates the index as a side effect of asking a
// question, and this runs before the caller has decided whether to commit.
func diffStats(dir string) (gitStats, error) {
	out, err := git(dir, "diff", "--numstat")
	if err != nil {
		return gitStats{}, err
	}
	untracked, err := git(dir, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return gitStats{}, err
	}
	var s gitStats
	for _, name := range strings.Fields(untracked) {
		n, err := countLines(filepath.Join(dir, name))
		if err != nil {
			return gitStats{}, err
		}
		s.files++
		s.add(name, n, 0)
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 {
			continue
		}
		s.files++
		added, aErr := strconv.Atoi(f[0])
		deleted, dErr := strconv.Atoi(f[1])
		if aErr != nil || dErr != nil {
			continue // binary; counted in files, not in lines
		}
		s.add(f[2], added, deleted)
	}
	return s, sc.Err()
}

// add routes one file's counts to the data or index tally.
func (s *gitStats) add(name string, added, deleted int) {
	if strings.HasSuffix(name, ".idx") {
		s.indexAdded += added
		s.indexDel += deleted
		return
	}
	s.dataAdded += added
	s.dataDel += deleted
}

// countLines counts newline-terminated lines, which is what a gitdb record file
// is: one record per line. A file not ending in a newline still counts its last
// partial line, matching what git reports as an added line.
func countLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var n int
	var trailing bool
	buf := make([]byte, 256*1024)
	for {
		c, err := f.Read(buf)
		for _, b := range buf[:c] {
			if b == '\n' {
				n++
				trailing = true
			} else {
				trailing = false
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
		if c == 0 {
			break
		}
	}
	if !trailing && n >= 0 {
		// unterminated final line
		fi, _ := f.Stat()
		if fi != nil && fi.Size() > 0 {
			n++
		}
	}
	return n, nil
}

// treeSize totals a directory RECURSIVELY. Distinct from dirSize in
// recompress.go, which reads only top-level entries: that is right for a flat
// store directory and wrong for .git, whose objects live in subdirectories.
// Reported as a delta around the commit, so a concurrent `git gc` shows up as a
// shrink rather than being mistaken for the commit's own cost.
func treeSize(dir string) int64 {
	var n int64
	filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
		return nil
	})
	return n
}

func humanBytes(n int64) string {
	switch {
	case n < 0:
		return "-" + humanBytes(-n)
	case n > 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n > 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
}

// reportAndCommit prints the diff the update produced and, if commit is set,
// records it. The diff is read BEFORE committing: afterwards the working tree is
// clean and the same question needs a different command, which is exactly the
// kind of asymmetry that produced mismatched numbers by hand.
func reportAndCommit(w io.Writer, records, message string, commit bool) error {
	if !isGitRepo(records) {
		if commit {
			return fmt.Errorf("-commit: %s is not a git work tree", records)
		}
		return nil
	}
	s, err := diffStats(records)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "\ngit: %d files changed\n", s.files)
	if s.empty() {
		fmt.Fprintln(w, "  nothing changed; no commit")
		return nil
	}
	fmt.Fprintf(w, "  .gitdb  +%d -%d  (appended)\n", s.dataAdded, s.dataDel)
	if s.indexAdded != 0 || s.indexDel != 0 {
		fmt.Fprintf(w, "  .idx    +%d -%d  (UNEXPECTED: format 6 stores hold no index files)\n",
			s.indexAdded, s.indexDel)
	}
	if s.dataDel != 0 {
		fmt.Fprintf(w, "  WARNING: %d deleted lines; an update should only append\n", s.dataDel)
	}

	if !commit {
		fmt.Fprintln(w, "  -commit not given; leaving the change uncommitted")
		return nil
	}

	gitDir := filepath.Join(records, ".git")
	before := treeSize(gitDir)
	if _, err := git(records, "add", "-A"); err != nil {
		return err
	}
	if _, err := git(records, "commit", "-q", "-m", message); err != nil {
		return err
	}
	after := treeSize(gitDir)
	rev, err := git(records, "rev-parse", "--short", "HEAD")
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "  committed %s  .git %s -> %s (%s)\n",
		strings.TrimSpace(rev), humanBytes(before), humanBytes(after),
		humanBytes(after-before))
	return nil
}

// defaultCommitMessage names the dump that was applied, so `git log` reads as a
// list of which days are in the store — the question anyone looking at this
// history will actually have.
func defaultCommitMessage(incr string) string {
	base := filepath.Base(incr)
	if i := strings.Index(base, "-pages-meta-hist-incr"); i > 0 {
		base = base[:i]
	}
	return fmt.Sprintf("Apply %s adds-changes", strings.TrimPrefix(base, "enwiki-"))
}
