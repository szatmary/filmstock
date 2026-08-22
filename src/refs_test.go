package main

import (
	"os"
	"strings"
	"testing"
)

// A self-closing <ref name="x"/> also matches reRef's opening tag, so if reRef
// runs first it treats the reuse marker as an opening <ref> and deletes
// everything up to the next real </ref>. On articles that reuse citations
// heavily that removed almost the entire body — silently, since the output is
// still valid text.
func TestSelfClosingRefDoesNotEatTheArticle(t *testing.T) {
	src := `Start of the article. <ref name="a"/> The plot follows a young wizard ` +
		`who discovers he is famous.<ref>{{cite web|title=Something}}</ref> ` +
		`He attends a school of magic and defeats a dark lord. End of article.`
	got := cleanText(src)
	for _, must := range []string{"young wizard", "school of magic", "End of article"} {
		if !strings.Contains(got, must) {
			t.Errorf("cleanText dropped %q\n  got: %s", must, got)
		}
	}
	if strings.Contains(got, "cite web") {
		t.Errorf("citation content leaked into the text: %s", got)
	}
}

func TestOrdinaryRefsStillStripped(t *testing.T) {
	got := cleanText(`Alpha<ref>noise one</ref> Beta<ref name="b">noise two</ref> Gamma`)
	for _, bad := range []string{"noise one", "noise two"} {
		if strings.Contains(got, bad) {
			t.Errorf("ref body survived: %s", got)
		}
	}
	for _, must := range []string{"Alpha", "Beta", "Gamma"} {
		if !strings.Contains(got, must) {
			t.Errorf("dropped %q from %q", must, got)
		}
	}
}

// The real article: 158k chars of wikitext that collapsed to 10k.
func TestRealArticleSurvivesCleaning(t *testing.T) {
	b, err := os.ReadFile("/tmp/claude-1000/-tank-filmstock/ba900659-9246-4446-869d-f16a7d1ad6bb/scratchpad/hpfilm.wiki")
	if err != nil {
		t.Skip("fixture missing")
	}
	out := fullPlainText(string(b))
	if len(out) < 30000 {
		t.Errorf("cleaned text is only %d chars from %d raw — refs are still eating the body",
			len(out), len(b))
	}
	if strings.HasPrefix(out, "{{Infobox") {
		t.Error("output starts with a raw infobox — the body was destroyed before stripping ran")
	}
	for _, must := range []string{"Hogwarts", "wizard"} {
		if !strings.Contains(out, must) {
			t.Errorf("expected %q in the cleaned article", must)
		}
	}
	t.Logf("cleaned: %d chars from %d raw", len(out), len(b))
}
