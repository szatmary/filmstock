package dump

import (
	"encoding/xml"
	"strings"
	"testing"
)

// The incremental (adds-changes) dumps are pages-meta-HIST: a page element
// carries every revision made in the window, not just the current one, about two
// per page on average. Page.Text is a scalar bound to revision>text, so which
// revision survives decoding is not a detail — it decides whether an incremental
// ingest writes the newest text or an intermediate one.
//
// Revisions appear in chronological order, so the answer must be the last.
func TestMultipleRevisionsKeepTheNewest(t *testing.T) {
	const doc = `<page>
  <title>Blade Runner</title>
  <ns>0</ns>
  <id>3746</id>
  <revision><id>1</id><timestamp>2026-08-21T10:00:00Z</timestamp><text>OLDEST</text></revision>
  <revision><id>2</id><timestamp>2026-08-21T11:00:00Z</timestamp><text>MIDDLE</text></revision>
  <revision><id>3</id><timestamp>2026-08-21T12:00:00Z</timestamp><text>NEWEST</text></revision>
</page>`
	var p Page
	if err := xml.NewDecoder(strings.NewReader(doc)).Decode(&p); err != nil {
		t.Fatal(err)
	}
	if p.ID != 3746 || p.Title != "Blade Runner" || p.NS != 0 {
		t.Fatalf("identity lost: %+v", p)
	}
	if p.Text != "NEWEST" {
		t.Fatalf("Text = %q, want NEWEST — an incremental ingest would write stale text", p.Text)
	}
}

// A single-revision page is the full-dump case and must be unaffected.
func TestSingleRevisionStillWorks(t *testing.T) {
	const doc = `<page><title>X</title><ns>0</ns><id>7</id>
  <revision><text>ONLY</text></revision></page>`
	var p Page
	if err := xml.NewDecoder(strings.NewReader(doc)).Decode(&p); err != nil {
		t.Fatal(err)
	}
	if p.Text != "ONLY" {
		t.Fatalf("Text = %q", p.Text)
	}
}

// RunStream must handle the shape an adds-changes dump actually has: a stream of
// pages, some with several revisions, wrapped in the same mediawiki envelope as
// the full dump.
func TestRunStreamOverAnAddsChangesShapedDocument(t *testing.T) {
	const doc = `<mediawiki version="0.11"><siteinfo><sitename>Wikipedia</sitename></siteinfo>
<page><title>A</title><ns>0</ns><id>1</id>
  <revision><text>a-old</text></revision>
  <revision><text>a-new</text></revision></page>
<page><title>Talk:A</title><ns>1</ns><id>2</id><revision><text>talk</text></revision></page>
<page><title>B</title><ns>0</ns><id>3</id><revision><text>b</text></revision></page>
</mediawiki>`
	var got []Page
	if err := RunStream(strings.NewReader(doc), func(p Page) { got = append(got, p) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d pages, want 3 (namespace filtering is the caller's job)", len(got))
	}
	if got[0].Text != "a-new" {
		t.Errorf("page A text = %q, want a-new", got[0].Text)
	}
	if got[1].NS != 1 || got[2].ID != 3 {
		t.Errorf("pages decoded out of shape: %+v", got)
	}
}
