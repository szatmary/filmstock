package main

import (
	"strings"
	"testing"
)

func words(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "w" + string(rune('a'+i%26))
	}
	return strings.Join(parts, " ")
}

func TestChunkCoversEveryWord(t *testing.T) {
	// Nothing may be dropped: a passage boundary must never delete prose, or the
	// missing sentence is silently unsearchable.
	src := words(1000)
	ps := chunkText(42, src, 100, 20)
	if len(ps) == 0 {
		t.Fatal("no passages")
	}
	covered := make([]bool, len(src))
	for _, p := range ps {
		if p.PageID != 42 {
			t.Fatalf("wrong page_id %d", p.PageID)
		}
		if got := src[p.Start : p.Start+len(p.Text)]; got != p.Text {
			t.Fatalf("passage %d Start offset does not point at its own text", p.Ord)
		}
		for i := p.Start; i < p.Start+len(p.Text); i++ {
			covered[i] = true
		}
	}
	for i, c := range covered {
		if !c && src[i] != ' ' {
			t.Fatalf("byte %d (%q) is in no passage", i, src[i])
		}
	}
}

func TestChunkOverlaps(t *testing.T) {
	// Overlap exists so a concept straddling a boundary still lands whole in at
	// least one window.
	ps := chunkText(1, words(500), 100, 20)
	if len(ps) < 2 {
		t.Fatalf("expected several passages, got %d", len(ps))
	}
	for i := 1; i < len(ps); i++ {
		prevEnd := ps[i-1].Start + len(ps[i-1].Text)
		if ps[i].Start >= prevEnd {
			t.Errorf("passage %d starts at %d, after previous ends at %d — no overlap",
				i, ps[i].Start, prevEnd)
		}
	}
}

func TestChunkShortDocIsOnePassage(t *testing.T) {
	// The median film article is ~307 words, so most documents are a single
	// passage and must not be split or padded.
	ps := chunkText(7, "a short plot summary of a film", 246, 49)
	if len(ps) != 1 {
		t.Fatalf("want 1 passage for a short doc, got %d", len(ps))
	}
	if ps[0].Text != "a short plot summary of a film" {
		t.Errorf("text altered: %q", ps[0].Text)
	}
}

func TestChunkEmptyAndWhitespace(t *testing.T) {
	for _, s := range []string{"", "   ", "\n\t\n"} {
		if ps := chunkText(1, s, 246, 49); len(ps) != 0 {
			t.Errorf("chunkText(%q) = %d passages, want 0", s, len(ps))
		}
	}
}

func TestChunkIsDeterministic(t *testing.T) {
	src := words(2000)
	a := chunkText(3, src, 246, 49)
	for i := 0; i < 5; i++ {
		b := chunkText(3, src, 246, 49)
		if len(a) != len(b) {
			t.Fatal("passage count varies between runs")
		}
		for j := range a {
			if a[j] != b[j] {
				t.Fatalf("passage %d differs between runs", j)
			}
		}
	}
}

func TestPageIDFromPath(t *testing.T) {
	if id, ok := pageIDFromPath("/tank/mediadb/out/text/07/30007.txt.gz"); !ok || id != 30007 {
		t.Errorf("got (%d,%v), want (30007,true)", id, ok)
	}
	if _, ok := pageIDFromPath("/tank/mediadb/out/text/07/not-a-number.txt.gz"); ok {
		t.Error("accepted a non-numeric filename")
	}
}
