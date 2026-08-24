package build

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/szatmary/filmstock"
)

// Bringing the store up to date meant knowing which adds-changes dumps were
// missing, fetching them in the right order, and applying each one by hand. Done
// from a shell script that is easy to get wrong in ways that are invisible
// afterwards: skip a day and the store silently carries a hole no later run will
// notice, because each day is applied on top of the last.
//
// catchup is that as one command, safe to run on a schedule.

const incrBase = "https://dumps.wikimedia.org/other/incr/enwiki/"

// Wikimedia keeps adds-changes dumps for about 42 days. Past that the only way
// back is a full dump plus the Wikidata pass, so falling behind is worth
// shouting about rather than discovering later.
const retentionDays = 42

var incrDayRe = regexp.MustCompile(`href="(\d{8})/"`)

func CmdCatchup(args []string) {
	fs := flag.NewFlagSet("catchup", flag.ExitOnError)
	records := fs.String("records", "filmstock-data", "record store to update in place")
	cache := fs.String("cache", defaultCachePath(), "Wikidata resolver cache")
	dumps := fs.String("dumps", "dump/incr", "where to keep downloaded dailies")
	from := fs.String("from", "", "first day to apply (YYYYMMDD); default is the day after the store's last")
	maxDays := fs.Int("max", 0, "stop after this many days (0 = all available)")
	commit := fs.Bool("commit", true, "commit each day to the record store's repository")
	keep := fs.Bool("keep", false, "keep the downloaded dumps instead of deleting each after use")
	dry := fs.Bool("dry-run", false, "list what would be applied and stop")
	fs.Parse(args)

	last := *from
	if last == "" {
		d, err := lastAppliedDay(*records)
		if err != nil {
			fatal(fmt.Errorf("cannot tell what the store already has: %w\n"+
				"pass -from YYYYMMDD to say explicitly", err))
		}
		last = nextDay(d)
		fmt.Fprintf(os.Stderr, "store is current through %s; starting at %s\n", d, last)
	}

	avail, err := availableDays()
	if err != nil {
		fatal(err)
	}
	var todo []string
	for _, d := range avail {
		if d >= last {
			todo = append(todo, d)
		}
	}
	if *maxDays > 0 && len(todo) > *maxDays {
		todo = todo[:*maxDays]
	}
	if len(todo) == 0 {
		fmt.Fprintln(os.Stderr, "already up to date")
		return
	}

	// A day older than retention that we still need means there is a hole no
	// daily dump can fill any more.
	if oldest := avail[0]; last < oldest {
		fmt.Fprintf(os.Stderr,
			"\nWARNING: the store needs %s but the oldest dump still published is %s.\n"+
				"  Those days have aged out (~%d day retention). The gap can only be closed\n"+
				"  by re-extracting from a full dump.\n\n", last, oldest, retentionDays)
	}

	fmt.Fprintf(os.Stderr, "%d day(s) to apply: %s .. %s\n", len(todo), todo[0], todo[len(todo)-1])
	if *dry {
		for _, d := range todo {
			fmt.Fprintf(os.Stderr, "  would apply %s\n", d)
		}
		return
	}

	if err := os.MkdirAll(*dumps, 0o777); err != nil {
		fatal(err)
	}
	start := time.Now()
	for i, d := range todo {
		path := filepath.Join(*dumps, incrName(d))
		fmt.Fprintf(os.Stderr, "\n[%d/%d] %s\n", i+1, len(todo), d)
		if err := fetchIncr(d, path); err != nil {
			// Stop rather than skip: applying days out of order would leave a
			// hole that nothing downstream would ever detect.
			fatal(fmt.Errorf("%s: %w\nstopping; the store is consistent through the previous day", d, err))
		}
		cargs := []string{"-incr", path, "-records", *records, "-cache", *cache}
		if *commit {
			cargs = append(cargs, "-commit")
		}
		CmdUpdate(cargs)
		if !*keep {
			os.Remove(path)
		}
	}
	fmt.Fprintf(os.Stderr, "\ncaught up: %d day(s) in %.1f min\n", len(todo), time.Since(start).Minutes())
}

// lastAppliedDay reads the most recent day out of the store repository's log,
// which is where `update -commit` records it.
func lastAppliedDay(records string) (string, error) {
	out, err := git(records, "log", "--format=%s", "-40")
	if err != nil {
		return "", err
	}
	re := regexp.MustCompile(`\b(20\d{6})\b`)
	for _, line := range strings.Split(out, "\n") {
		if m := re.FindStringSubmatch(line); m != nil {
			return m[1], nil
		}
	}
	return "", fmt.Errorf("no commit in %s names a dump date", records)
}

func availableDays() ([]string, error) {
	resp, err := httpGet(incrBase)
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", incrBase, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range incrDayRe.FindAllStringSubmatch(string(body), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no dump directories found at %s", incrBase)
	}
	sort.Strings(out)
	return out, nil
}

func incrName(day string) string {
	return fmt.Sprintf("enwiki-%s-pages-meta-hist-incr.xml.bz2", day)
}

// fetchIncr downloads a day's dump if it is not already present and complete.
// A dump that is still being generated is served with the wrong size or not at
// all; a short read here is a stop, not something to work around.
func fetchIncr(day, path string) error {
	url := incrBase + day + "/" + incrName(day)
	resp, err := httpDo("HEAD", url)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	want := resp.ContentLength
	if fi, err := os.Stat(path); err == nil && (want < 0 || fi.Size() == want) {
		fmt.Fprintf(os.Stderr, "  have %s (%.0f MB)\n", filepath.Base(path), float64(fi.Size())/(1<<20))
		return nil
	}
	fmt.Fprintf(os.Stderr, "  fetching %.0f MB\n", float64(want)/(1<<20))
	get, err := httpGet(url)
	if err != nil {
		return err
	}
	defer get.Body.Close()
	if get.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, get.Status)
	}
	tmp := path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, get.Body)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if want > 0 && n != want {
		os.Remove(tmp)
		return fmt.Errorf("short read: got %d bytes, want %d", n, want)
	}
	return os.Rename(tmp, path)
}

// Wikimedia rejects the Go client's default user agent, and asks tools to
// identify themselves. Without this the dump listing comes back empty, which
// looks exactly like "no dumps published" rather than like being turned away.
func httpDo(method, url string) (*http.Response, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", filmstock.UserAgent)
	return http.DefaultClient.Do(req)
}

func httpGet(url string) (*http.Response, error) { return httpDo("GET", url) }

func defaultCachePath() string {
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "filmstock", "resolver.db")
	}
	return "resolver.db"
}

// nextDay advances a YYYYMMDD string by one day.
func nextDay(d string) string {
	t, err := time.Parse("20060102", d)
	if err != nil {
		return d
	}
	return t.AddDate(0, 0, 1).Format("20060102")
}
