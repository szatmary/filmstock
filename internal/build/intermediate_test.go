package build

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openInter(t *testing.T) *Inter {
	t.Helper()
	in, err := OpenInter(filepath.Join(t.TempDir(), "inter.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close() })
	return in
}

func TestInterRoundTrip(t *testing.T) {
	in := openInter(t)
	err := in.Put(&Page{
		PageID: 3746, Kind: "movies", Title: "Blade Runner",
		Wikitext: "{{Infobox film|director=[[Ridley Scott]]}}",
		Infobox:  map[string]string{"director": "[[Ridley Scott]]"},
		Lead:     "Blade Runner is a 1982 science fiction film.",
		Links:    []PageLink{{Field: "director", Target: "Ridley Scott"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var seen int
	err = in.Each("movies", true, func(p *Page) error {
		seen++
		if p.PageID != 3746 || p.Title != "Blade Runner" {
			t.Errorf("got %d %q", p.PageID, p.Title)
		}
		if p.Infobox["director"] != "[[Ridley Scott]]" {
			t.Errorf("infobox lost: %v", p.Infobox)
		}
		if p.Wikitext == "" {
			t.Error("wikitext not stored — a parser fix would need the dump again")
		}
		return nil
	})
	if err != nil || seen != 1 {
		t.Fatalf("visited %d pages, err %v", seen, err)
	}
}

// Wikitext is the bulk of the store, so most passes must be able to skip it.
func TestInterCanSkipWikitext(t *testing.T) {
	in := openInter(t)
	in.Put(&Page{PageID: 1, Kind: "movies", Title: "A", Wikitext: "a lot of markup"})
	in.Each("movies", false, func(p *Page) error {
		if p.Wikitext != "" {
			t.Error("wikitext was read when not asked for")
		}
		return nil
	})
}

// Re-importing must replace, not duplicate: a daily update is an upsert and a
// full re-import must be idempotent.
func TestInterPutIsIdempotent(t *testing.T) {
	in := openInter(t)
	for i := range 3 {
		if err := in.Put(&Page{PageID: 7, Kind: "movies", Title: "Heat",
			Lead: "revision " + string(rune('A'+i))}); err != nil {
			t.Fatal(err)
		}
	}
	c, err := in.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if c["movies"] != 1 {
		t.Errorf("movies = %d, want 1 — repeated Put must replace", c["movies"])
	}
	in.Each("movies", false, func(p *Page) error {
		if p.Lead != "revision C" {
			t.Errorf("kept %q, want the newest revision", p.Lead)
		}
		return nil
	})
}

// One page can be claimed by more than one recogniser — an article may carry both
// a film infobox and a person infobox — and deciding which wins is export's job.
func TestInterKeepsBothKindsOfOnePage(t *testing.T) {
	in := openInter(t)
	in.Put(&Page{PageID: 42, Kind: "movies", Title: "X"})
	in.Put(&Page{PageID: 42, Kind: "people", Title: "X"})
	c, _ := in.Counts()
	if c["movies"] != 1 || c["people"] != 1 {
		t.Errorf("counts = %v, want one of each", c)
	}
}

// The reference graph is what makes incremental export possible: adding a film
// must mark its cast stale, and without recorded references nothing can know.
func TestInterReferrers(t *testing.T) {
	in := openInter(t)
	in.Put(&Page{PageID: 1, Kind: "movies", Title: "Heat",
		Links: []PageLink{{"director", "Michael Mann"}, {"starring", "Al Pacino"}}})
	in.Put(&Page{PageID: 2, Kind: "movies", Title: "Collateral",
		Links: []PageLink{{"director", "Michael Mann"}}})
	in.Flush()

	got, err := in.Referrers("Michael Mann")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("Michael Mann referred to by %v, want both films", got)
	}
	if got, _ := in.Referrers("Al Pacino"); len(got) != 1 {
		t.Errorf("Al Pacino referred to by %v, want one", got)
	}
	if got, _ := in.Referrers("Nobody"); len(got) != 0 {
		t.Errorf("unknown target has referrers: %v", got)
	}
}

func TestInterByTitle(t *testing.T) {
	in := openInter(t)
	in.Put(&Page{PageID: 9, Kind: "television", Title: "ER (TV series)"})
	in.Flush()
	if id, ok := in.ByTitle("ER (TV series)", "television"); !ok || id != 9 {
		t.Errorf("ByTitle = %d,%v want 9,true", id, ok)
	}
	if _, ok := in.ByTitle("ER (TV series)", "movies"); ok {
		t.Error("resolved against the wrong kind")
	}
}

// A stale intermediate mixed with a newer dump would be silently wrong.
func TestInterRecordsItsSource(t *testing.T) {
	in := openInter(t)
	if err := in.SetSource("enwiki-20260801"); err != nil {
		t.Fatal(err)
	}
	got, err := in.Source()
	if err != nil || got != "enwiki-20260801" {
		t.Errorf("Source = %q, %v", got, err)
	}
}

// A film states many starring links. Anything keyed by field alone keeps one.
func TestInterKeepsEveryLinkInAField(t *testing.T) {
	in := openInter(t)
	err := in.Put(&Page{PageID: 1, Kind: "movies", Title: "Heat", Links: []PageLink{
		{"starring", "Al Pacino"},
		{"starring", "Robert De Niro"},
		{"starring", "Val Kilmer"},
		{"director", "Michael Mann"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	in.Flush()
	for _, who := range []string{"Al Pacino", "Robert De Niro", "Val Kilmer", "Michael Mann"} {
		got, err := in.Referrers(who)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Errorf("%s: %d referrers, want 1 — a cast member was dropped", who, len(got))
		}
	}
}

// One person credited twice on the same film keeps both roles: writer-directors
// are common and export needs to know they did both.
func TestInterKeepsOnePersonInTwoFields(t *testing.T) {
	in := openInter(t)
	in.Put(&Page{PageID: 1, Kind: "movies", Title: "X", Links: []PageLink{
		{"director", "Quentin Tarantino"},
		{"writer", "Quentin Tarantino"},
	}})
	in.Flush()
	var n int
	rows, err := in.db.Query(`SELECT field FROM links WHERE target = ?`, "Quentin Tarantino")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		n++
	}
	if n != 2 {
		t.Errorf("kept %d roles, want 2 (director and writer)", n)
	}
}

// Wikitext is stored compressed; it must come back byte-identical, since the
// whole point of keeping it is re-parsing without the dump.
func TestInterWikitextSurvivesCompression(t *testing.T) {
	in := openInter(t)
	text := strings.Repeat("{{Infobox film\n| name = Heat\n}}\n''Heat'' is a 1995 film.\n", 200)
	if err := in.Put(&Page{PageID: 1, Kind: "movies", Title: "Heat", Wikitext: text}); err != nil {
		t.Fatal(err)
	}
	in.Flush()
	var got string
	in.Each("movies", true, func(p *Page) error { got = p.Wikitext; return nil })
	if got != text {
		t.Fatalf("wikitext round-trip lost data: %d bytes back, %d in", len(got), len(text))
	}
	var stored int
	in.db.QueryRow(`SELECT LENGTH(wikitext) FROM pages WHERE page_id=1`).Scan(&stored)
	if stored >= len(text) {
		t.Errorf("stored %d bytes for %d of text — not compressed", stored, len(text))
	}
}

// A store written before wikitext was compressed still reads: the blob is
// self-describing via gzip's magic bytes.
func TestInterReadsUncompressedWikitext(t *testing.T) {
	in := openInter(t)
	in.Put(&Page{PageID: 1, Kind: "movies", Title: "Heat"})
	in.Flush()
	if _, err := in.db.Exec(`UPDATE pages SET wikitext=? WHERE page_id=1`, []byte("plain text")); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := in.Each("movies", true, func(p *Page) error { got = p.Wikitext; return nil }); err != nil {
		t.Fatal(err)
	}
	if got != "plain text" {
		t.Errorf("got %q, want the uncompressed blob back", got)
	}
}

// Reading while a write transaction is open must not deadlock. The store holds
// one connection; a read that goes to the pool instead of the transaction waits
// on a connection the transaction is holding, forever. The daily update reads
// and writes interleaved, page by page, so this is the normal path.
func TestReadDuringOpenTransaction(t *testing.T) {
	in := openInter(t)
	if err := in.Put(&Page{PageID: 1, Kind: "movies", Title: "Heat"}); err != nil {
		t.Fatal(err)
	}
	// No Flush: the transaction is deliberately still open.
	done := make(chan error, 1)
	go func() {
		_, err := in.Kinds(1)
		if err == nil {
			_, err = in.Referrers("x")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read deadlocked against the open write transaction")
	}
}

// A page that stops qualifying must stop being stored. An infobox is removed,
// so a film is no longer a film; Put writes nothing and the old row survives,
// leaving the store asserting something the encyclopaedia no longer says.
func TestReplaceDropsKindsNoLongerClaimed(t *testing.T) {
	in := openInter(t)
	in.Put(&Page{PageID: 1, Kind: "movies", Title: "Heat"})
	in.Put(&Page{PageID: 1, Kind: "people", Title: "Heat"})
	in.Flush()

	// Re-imported, now claimed only as a person.
	if err := in.Replace(1, []*Page{{PageID: 1, Kind: "people", Title: "Heat"}}); err != nil {
		t.Fatal(err)
	}
	in.Flush()
	got, err := in.Kinds(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "people" {
		t.Errorf("kinds %v, want [people] — a stale claim survived", got)
	}
}

// A page claimed by nothing at all is removed entirely.
func TestReplaceWithNoClaimsRemovesThePage(t *testing.T) {
	in := openInter(t)
	in.Put(&Page{PageID: 1, Kind: "movies", Title: "Heat",
		Links: []PageLink{{"director", "Michael Mann"}}})
	in.Flush()
	if err := in.Replace(1, nil); err != nil {
		t.Fatal(err)
	}
	in.Flush()
	if got, _ := in.Kinds(1); len(got) != 0 {
		t.Errorf("page still stored as %v", got)
	}
	if got, _ := in.Referrers("Michael Mann"); len(got) != 0 {
		t.Errorf("links survived the page: %v", got)
	}
}

// Links are rewritten wholesale. A cast member removed from an infobox must
// leave the reference graph, which is what export uses to decide what is stale.
func TestReplaceClearsStaleLinks(t *testing.T) {
	in := openInter(t)
	in.Put(&Page{PageID: 1, Kind: "movies", Title: "Heat", Links: []PageLink{
		{"starring", "Al Pacino"}, {"starring", "Robert De Niro"}}})
	in.Flush()
	if err := in.Replace(1, []*Page{{PageID: 1, Kind: "movies", Title: "Heat",
		Links: []PageLink{{"starring", "Al Pacino"}}}}); err != nil {
		t.Fatal(err)
	}
	in.Flush()
	if got, _ := in.Referrers("Robert De Niro"); len(got) != 0 {
		t.Errorf("removed cast member still in the reference graph: %v", got)
	}
	if got, _ := in.Referrers("Al Pacino"); len(got) != 1 {
		t.Errorf("kept cast member lost from the graph: %v", got)
	}
}

// Replace updates in place rather than duplicating.
func TestReplaceUpdatesContent(t *testing.T) {
	in := openInter(t)
	in.Put(&Page{PageID: 1, Kind: "movies", Title: "Heat", Wikitext: "old"})
	in.Flush()
	in.Replace(1, []*Page{{PageID: 1, Kind: "movies", Title: "Heat (1995 film)", Wikitext: "new"}})
	in.Flush()
	var n int
	var title, text string
	in.db.QueryRow(`SELECT COUNT(*) FROM pages WHERE page_id=1`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d rows, want 1", n)
	}
	in.Each("movies", true, func(p *Page) error { title, text = p.Title, p.Wikitext; return nil })
	if title != "Heat (1995 film)" || text != "new" {
		t.Errorf("got %q/%q, want the updated values", title, text)
	}
}
