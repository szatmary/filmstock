package build

import (
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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

// CmdCatchup runs the daily update once per day the store is behind.
//
// A day at a time, deliberately, even when 23 days behind. Applying them in one
// batch and exporting once would be faster, and would exercise a path that runs
// only while the project is being built and never again. The daily job is what
// will run every day forever, so that is the job that gets run.
func CmdCatchup(args []string) {
	fs := flag.NewFlagSet("catchup", flag.ExitOnError)
	dbOut := fs.String("db", "", "SQLite database to publish each day")
	textOut := fs.String("text-db", "", "synopsis database (default <db>-text.db)")
	cache := fs.String("cache", defaultCachePath(), "Wikidata resolver cache")
	dumps := fs.String("dumps", "dump/incr", "where to keep downloaded dailies")
	full := fs.String("full-dumps", "dump", "directory holding the full dump set")
	inter := fs.String("inter", defaultInterPath(), "intermediate store the days are applied to")
	workers := fs.Int("workers", 18, "parallel workers")
	from := fs.String("from", "", "first day to apply (YYYYMMDD); default is the day after the store's last")
	maxDays := fs.Int("max", 0, "stop after this many days (0 = all available)")
	keep := fs.Bool("keep", false, "keep the downloaded dumps instead of deleting each after use")
	dry := fs.Bool("dry-run", false, "list what would be applied and stop")
	fs.Parse(args)

	if *dbOut == "" {
		fatal(fmt.Errorf("catchup needs -db FILE"))
	}
	last := *from
	if last == "" {
		d, err := lastAppliedDay(*inter)
		if err != nil {
			fatal(fmt.Errorf("cannot tell what the intermediate already has: %w\n"+
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
		cargs := []string{"-incr", path, "-db", *dbOut, "-text-db", *textOut,
			"-cache", *cache, "-inter", *inter, "-dumps", *full,
			"-workers", strconv.Itoa(*workers)}
		CmdUpdate(cargs)
		if !*keep {
			os.Remove(path)
		}
	}
	fmt.Fprintf(os.Stderr, "\ncaught up: %d day(s) in %.1f min\n", len(todo), time.Since(start).Minutes())
}

// lastAppliedDay reads how far the intermediate has advanced: the last
// adds-changes day importIncr stamped, or failing that the day of the full
// dump it was imported from. Before the record store was retired this came
// from the store repository's git log.
func lastAppliedDay(interPath string) (string, error) {
	in, err := OpenInter(interPath)
	if err != nil {
		return "", err
	}
	defer in.Close()
	if day, err := in.Meta("incr_through"); err != nil {
		return "", err
	} else if day != "" {
		return day, nil
	}
	src, err := in.Meta("source")
	if err != nil {
		return "", err
	}
	if day := dayFromDumpName(src); day != "" {
		return day, nil
	}
	return "", fmt.Errorf("%s states no incr_through and its source %q names no date", interPath, src)
}

// dayFromDumpName pulls the YYYYMMDD out of a dump filename
// (enwiki-20260801-… or a daily 20260815/enwiki-….xml.bz2 path).
func dayFromDumpName(path string) string {
	if m := regexp.MustCompile(`\b(20\d{6})\b`).FindStringSubmatch(path); m != nil {
		return m[1]
	}
	return ""
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

// dumpClient deliberately has no overall Timeout: a daily dump is ~800 MB and a
// slow mirror can legitimately take many minutes to send it. What it does bound
// is everything before the body starts flowing, so a dead or hanging server
// fails in seconds instead of stalling a scheduled run until the CI job's own
// limit kills it hours later.
var dumpClient = &http.Client{
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	},
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
	return dumpClient.Do(req)
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
