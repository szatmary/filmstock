package build

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/szatmary/filmstock"
)

func TestNextDayCrossesBoundaries(t *testing.T) {
	for _, c := range [][2]string{
		{"20260822", "20260823"},
		{"20260831", "20260901"}, // month
		{"20261231", "20270101"}, // year
		{"20240228", "20240229"}, // leap
		{"20250228", "20250301"}, // non-leap
	} {
		if got := nextDay(c[0]); got != c[1] {
			t.Errorf("nextDay(%s) = %s, want %s", c[0], got, c[1])
		}
	}
}

// The store's own git log is the record of what it already holds. Reading the
// wrong day here would re-apply days or skip them.
func TestLastAppliedDayReadsTheLog(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "f", "x")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "filmstock record store: 2026-08-01 enwiki dump")
	for _, d := range []string{"20260801", "20260802", "20260803"} {
		write(t, dir, "f", d)
		gitT(t, dir, "add", "-A")
		gitT(t, dir, "commit", "-q", "-m", "Apply "+d+" adds-changes")
	}
	got, err := lastAppliedDay(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "20260803" {
		t.Errorf("lastAppliedDay = %s, want 20260803 (the most recent)", got)
	}
}

// Better to refuse than to guess: guessing wrong silently skips days, and a
// skipped day leaves a hole no later run detects.
func TestLastAppliedDayRefusesWhenNoCommitNamesADay(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "f", "x")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "initial import")
	if _, err := lastAppliedDay(dir); err == nil {
		t.Error("expected an error when no commit names a dump date")
	}
}

func TestIncrNameMatchesWikimedia(t *testing.T) {
	want := "enwiki-20260822-pages-meta-hist-incr.xml.bz2"
	if got := incrName("20260822"); got != want {
		t.Errorf("incrName = %q, want %q", got, want)
	}
}

// The listing regex must match the real page markup. When it silently matched
// nothing, the failure read as "no dumps published" rather than "we were turned
// away for our user agent".
func TestIncrDayRegexMatchesRealListing(t *testing.T) {
	page := `<a href="../">../</a>
<a href="20260713/">20260713/</a>   06-Jul-2026 09:12    -
<a href="20260822/">20260822/</a>   22-Aug-2026 09:41    -`
	m := incrDayRe.FindAllStringSubmatch(page, -1)
	if len(m) != 2 {
		t.Fatalf("matched %d days, want 2", len(m))
	}
	if m[0][1] != "20260713" || m[1][1] != "20260822" {
		t.Errorf("matched %v", m)
	}
}

// A partial download must never be left where it would be mistaken for a
// complete dump on the next run.
func TestFetchIncrLeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	// A .part file from an interrupted run must not be treated as the dump.
	part := filepath.Join(dir, incrName("20260822")+".part")
	if err := os.WriteFile(part, []byte("truncated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, incrName("20260822"))); err == nil {
		t.Error("a .part file must not satisfy the presence check")
	}
}

// Wikimedia turns away Go's default user agent, and the refusal is invisible:
// the dump listing simply comes back empty, which reads as "no dumps published".
// This asserts the header is actually on the wire.
func TestRequestsCarryTheUserAgent(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	resp, err := httpGet(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got == "" {
		t.Fatal("no User-Agent sent")
	}
	if strings.HasPrefix(got, "Go-http-client") {
		t.Fatalf("sent the default agent %q; Wikimedia rejects it", got)
	}
	if got != filmstock.UserAgent {
		t.Errorf("User-Agent = %q, want %q", got, filmstock.UserAgent)
	}
}

// A hung mirror must not stall a scheduled run until CI's own limit kills it.
func TestDumpClientBoundsTheHandshakeNotTheBody(t *testing.T) {
	if dumpClient.Timeout != 0 {
		t.Error("an overall timeout would abort legitimate multi-minute 800 MB downloads")
	}
	tr, ok := dumpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected a configured Transport")
	}
	if tr.ResponseHeaderTimeout == 0 || tr.TLSHandshakeTimeout == 0 {
		t.Error("a dead server must fail fast, before the body starts")
	}
}
