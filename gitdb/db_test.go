package gitdb

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func open(t *testing.T, dir string, opts ...Option) *DB {
	t.Helper()
	db, err := Open(dir, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func put(t *testing.T, db *DB, key, val string) {
	t.Helper()
	if err := db.PutString(key, val); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func get(t *testing.T, db *DB, key string) string {
	t.Helper()
	b, err := db.Get(key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	return string(b)
}

func TestPutGet(t *testing.T) {
	db := open(t, t.TempDir())
	put(t, db, "12345", `{"title":"Heat"}`)
	if got := get(t, db, "12345"); got != `{"title":"Heat"}` {
		t.Errorf("got %q", got)
	}
	if _, err := db.Get("nope"); err != ErrNotFound {
		t.Errorf("missing key: err = %v, want ErrNotFound", err)
	}
}

// The point of keying by the caller's identity: the key a consumer already has
// (a page_id) is the key, with no mapping layer to keep in sync.
func TestPutSupersedesAndBumpsVersion(t *testing.T) {
	db := open(t, t.TempDir())
	put(t, db, "42", "one")
	if v, _ := db.Version("42"); v != 1 {
		t.Errorf("first version = %d, want 1", v)
	}
	put(t, db, "42", "two")
	if v, _ := db.Version("42"); v != 2 {
		t.Errorf("second version = %d, want 2", v)
	}
	if got := get(t, db, "42"); got != "two" {
		t.Errorf("got %q, want the newer value", got)
	}
	if n := db.Len(); n != 1 {
		t.Errorf("Len = %d, want 1 — a superseded line is not a second record", n)
	}
}

// Updating appends. Nothing already written moves, which is the property the
// whole format exists for: a day's changes must be a pure append in git.
func TestUpdateOnlyAppends(t *testing.T) {
	dir := t.TempDir()
	db := open(t, dir)
	put(t, db, "1", "aaaa")
	put(t, db, "2", "bbbb")
	before, err := os.ReadFile(db.path(1))
	if err != nil {
		t.Fatal(err)
	}
	put(t, db, "1", "cccc")
	after, err := os.ReadFile(db.path(1))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(after), string(before)) {
		t.Fatal("existing bytes changed; the update was not a pure append")
	}
}

func TestReopenRebuildsFromScan(t *testing.T) {
	dir := t.TempDir()
	db := open(t, dir)
	for i := range 50 {
		put(t, db, fmt.Sprint(i), fmt.Sprintf("value-%d", i))
	}
	put(t, db, "7", "updated")
	if err := db.Delete("9"); err != nil {
		t.Fatal(err)
	}

	// No index file exists; the reopened db must reconstruct everything.
	db2 := open(t, dir)
	if got := get(t, db2, "7"); got != "updated" {
		t.Errorf("after reopen, key 7 = %q, want the updated value", got)
	}
	if _, err := db2.Get("9"); err != ErrNotFound {
		t.Errorf("deleted key came back after reopen: %v", err)
	}
	if db2.Len() != 49 {
		t.Errorf("Len after reopen = %d, want 49", db2.Len())
	}
	if v, _ := db2.Version("7"); v != 2 {
		t.Errorf("version after reopen = %d, want 2", v)
	}
}

func TestNoIndexFilesAreWritten(t *testing.T) {
	dir := t.TempDir()
	db := open(t, dir)
	for i := range 20 {
		put(t, db, fmt.Sprint(i), strings.Repeat("x", 100))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), fileExt) {
			t.Errorf("unexpected file %q — the store must be record files only", e.Name())
		}
	}
}

func TestDeleteThenPutResurrects(t *testing.T) {
	db := open(t, t.TempDir())
	put(t, db, "5", "first")
	if err := db.Delete("5"); err != nil {
		t.Fatal(err)
	}
	if db.Has("5") {
		t.Fatal("key still present after delete")
	}
	if err := db.Delete("5"); err != ErrNotFound {
		t.Errorf("second delete: %v, want ErrNotFound", err)
	}
	put(t, db, "5", "second")
	if got := get(t, db, "5"); got != "second" {
		t.Errorf("got %q", got)
	}
}

// Version must keep increasing across a delete, or a resurrected key could be
// resolved to its pre-delete line by a scan that takes the highest version.
func TestVersionIncreasesAcrossDelete(t *testing.T) {
	dir := t.TempDir()
	db := open(t, dir)
	put(t, db, "5", "first")  // v1
	db.Delete("5")            // v2
	put(t, db, "5", "second") // v3
	if v, _ := db.Version("5"); v != 3 {
		t.Errorf("version = %d, want 3", v)
	}
	if got := get(t, open(t, dir), "5"); got != "second" {
		t.Errorf("after reopen got %q, want %q", got, "second")
	}
}

func TestRollsToNewFileAtCap(t *testing.T) {
	dir := t.TempDir()
	db := open(t, dir, WithMaxFileSize(2048))
	for i := range 40 {
		put(t, db, fmt.Sprint(i), incompressible(i, 200))
	}
	files, err := db.recordFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 2 {
		t.Fatalf("expected a rollover, got %d file(s)", len(files))
	}
	for _, i := range files {
		fi, err := os.Stat(db.path(i))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() > 2048 {
			t.Errorf("file %d is %d bytes, over the 2048 cap", i, fi.Size())
		}
	}
	// Everything must still be readable across the rollover.
	db2 := open(t, dir, WithMaxFileSize(2048))
	if db2.Len() != 40 {
		t.Errorf("Len = %d, want 40", db2.Len())
	}
}

// Every new file needs its own header, or a scan of it fails.
func TestEveryFileHasAHeader(t *testing.T) {
	dir := t.TempDir()
	db := open(t, dir, WithMaxFileSize(2048))
	for i := range 40 {
		put(t, db, fmt.Sprint(i), incompressible(i, 200))
	}
	files, _ := db.recordFiles()
	for _, i := range files {
		f, err := os.Open(db.path(i))
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Scan()
		line := sc.Text()
		f.Close()
		if !strings.HasPrefix(line, headerMagic+" "+formatVersion+" ") {
			t.Errorf("file %d header = %q", i, line)
		}
	}
}

func TestAllYieldsOnlyCurrentVersions(t *testing.T) {
	dir := t.TempDir()
	db := open(t, dir)
	put(t, db, "1", "a1")
	put(t, db, "2", "b1")
	put(t, db, "3", "c1")
	put(t, db, "1", "a2") // supersedes
	db.Delete("2")        // removes

	got := map[string]string{}
	for rec, err := range db.All() {
		if err != nil {
			t.Fatal(err)
		}
		if _, dup := got[rec.Key]; dup {
			t.Errorf("key %s yielded twice", rec.Key)
		}
		got[rec.Key] = string(rec.Data)
	}
	want := map[string]string{"1": "a2", "3": "c1"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("key %s = %q, want %q", k, got[k], v)
		}
	}
}

func TestRejectsNewlineInRecord(t *testing.T) {
	db := open(t, t.TempDir())
	if err := db.PutString("1", "line\nbreak"); err == nil {
		t.Error("a record containing a newline must be rejected")
	}
}

// A key with a space would be silently truncated by the scanner, turning into a
// different key on reopen. It has to be refused at write time.
func TestRejectsBadKeys(t *testing.T) {
	db := open(t, t.TempDir())
	for _, k := range []string{"", "has space", "has\ttab", "has\nnewline"} {
		if err := db.PutString(k, "x"); err == nil {
			t.Errorf("key %q was accepted", k)
		}
	}
}

func TestDictionaryMismatchIsRefused(t *testing.T) {
	dir := t.TempDir()
	db := open(t, dir, WithDictionary([]byte("dictionary one")))
	put(t, db, "1", "hello")

	if _, err := Open(dir, WithDictionary([]byte("a different dictionary"))); err == nil {
		t.Fatal("opening with the wrong dictionary must fail, not inflate to garbage")
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("opening with no dictionary must fail")
	}
}

func TestRoundTripsWithDictionary(t *testing.T) {
	dir := t.TempDir()
	dict := []byte(`{"title":"","year":,"directors":[],"cast":[]}`)
	db := open(t, dir, WithDictionary(dict), WithLevel(9))
	body := `{"title":"Heat","year":1995,"directors":["Michael Mann"],"cast":["Pacino"]}`
	put(t, db, "12345", body)
	if got := get(t, open(t, dir, WithDictionary(dict), WithLevel(9)), "12345"); got != body {
		t.Errorf("round trip failed: %q", got)
	}
}

func TestScanRejectsCorruptLine(t *testing.T) {
	dir := t.TempDir()
	db := open(t, dir)
	put(t, db, "1", "hello")
	f, err := os.OpenFile(db.path(1), os.O_APPEND|os.O_WRONLY, 0o666)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("this-line-has-no-version-or-payload\n")
	f.Close()
	if _, err := Open(dir); err == nil {
		t.Error("a malformed line must fail the scan rather than be skipped")
	}
}

func TestInterruptedCompactionIsRefused(t *testing.T) {
	dir := t.TempDir()
	db := open(t, dir)
	put(t, db, "1", "hello")
	if err := os.WriteFile(filepath.Join(dir, compactMarker), []byte("x"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Error("a leftover compaction marker must block Open")
	}
}

// incompressible builds a payload that zlib cannot shrink, so a size cap is
// actually reached. Repeated text compresses to almost nothing, which is why the
// first version of the rollover test never rolled over.
func incompressible(seed, n int) string {
	var b strings.Builder
	x := uint32(seed*2654435761 + 1)
	for b.Len() < n {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		fmt.Fprintf(&b, "%08x", x)
	}
	return b.String()[:n]
}
