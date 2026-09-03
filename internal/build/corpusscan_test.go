package build

// Corpus validation for the episode fields, run against the intermediate
// database rather than a fixture, because the fixtures are the cases we already
// know about. Skips unless -corpus is given:
//
//	go test ./internal/build/ -run TestCorpusScan -v -timeout 30m \
//	    -corpus=/tank/mediadb/intermediate-v3.db
//
// It earned its place: on the first run it found that splitting a production
// code on <br /> cut citations in half, that commented-out parameters were
// being published as codes, and that parameters truncated at a pipe inside a
// citation left the citation's prose behind. None of those were visible in the
// fixtures. Takes about 100s over 691k episodes.

import (
	"compress/gzip"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/szatmary/filmstock/internal/dump"
	"github.com/szatmary/filmstock/internal/sqldrv"
)

var corpusPath = flag.String("corpus", "", "path to intermediate db for corpus validation")

func TestCorpusScan(t *testing.T) {
	if *corpusPath == "" {
		t.Skip("no -corpus given")
	}
	db, err := sql.Open(sqldrv.Name, *corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT page_id, title, wikitext FROM pages
	   WHERE kind IN ('episode_lists','seasons','television')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	start := time.Now()
	var pages, eps, withProd, multiProd, dirty int
	var airChanged, airGained, airLost int
	prodLen := map[int]int{}
	sample := map[string][]string{}
	add := func(k, s string, max int) {
		if len(sample[k]) < max {
			sample[k] = append(sample[k], s)
		}
	}
	for rows.Next() {
		var id int
		var title string
		var blob []byte
		if err := rows.Scan(&id, &title, &blob); err != nil {
			t.Fatal(err)
		}
		zr, err := gzip.NewReader(strings.NewReader(string(blob)))
		if err != nil {
			continue
		}
		raw, err := io.ReadAll(zr)
		zr.Close()
		if err != nil {
			continue
		}
		text := string(raw)
		if !strings.Contains(strings.ToLower(text), "{{episode list") {
			continue
		}
		pages++
		for _, m := range parseTelevisionPage(dump.Page{ID: id, Title: title, Text: text}) {
			for _, e := range m.eps {
				eps++
				if e.ProdCode != "" {
					withProd++
					if strings.Contains(e.ProdCode, " / ") {
						multiProd++
					}
					prodLen[len(e.ProdCode)]++
					if strings.ContainsAny(e.ProdCode, "{}[]<>|") {
						dirty++
						add("markup left in code", title+" :: "+e.ProdCode, 25)
					}
					if len(e.ProdCode) > 40 {
						add("suspiciously long code", title+" :: "+e.ProdCode, 20)
					}
				}
			}
		}
		// What the air-date change actually did, measured against the old rule
		// of taking whatever date came first.
		for _, v := range airDateFieldsOf(text) {
			old := ""
			if d := parseReleaseDates(v); len(d) > 0 {
				old = d[0]
			}
			neu := episodeAirDate(v)
			switch {
			case old == neu:
			case old != "" && neu == "":
				airLost++
				add("air date LOST", title+" :: "+trunc(v), 20)
			case old == "" && neu != "":
				airGained++
				add("air date gained", title+" :: "+trunc(v), 20)
			default:
				airChanged++
				add("air date changed", fmt.Sprintf("%s :: %s -> %s :: %s",
					title, old, neu, trunc(v)), 30)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	t.Logf("elapsed %s", time.Since(start).Round(time.Millisecond))
	t.Logf("pages=%d episodes=%d withProdCode=%d (%.1f%%) multiCode=%d",
		pages, eps, withProd, 100*float64(withProd)/float64(max(eps, 1)), multiProd)
	t.Logf("air dates: changed=%d gained=%d lost=%d", airChanged, airGained, airLost)
	t.Logf("codes still carrying markup: %d", dirty)

	var lens []int
	for l := range prodLen {
		lens = append(lens, l)
	}
	sort.Ints(lens)
	var b strings.Builder
	for _, l := range lens {
		fmt.Fprintf(&b, "%d:%d ", l, prodLen[l])
	}
	t.Logf("prod code lengths: %s", b.String())

	keys := make([]string, 0, len(sample))
	for k := range sample {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("--- %s (%d shown) ---", k, len(sample[k]))
		for _, s := range sample[k] {
			t.Logf("   %s", s)
		}
	}
	// Not zero: a handful of articles state a broken code and reproducing it is
	// correct. "List of Batman: The Brave and the Bold episodes" really does
	// say "| ProdCode = 305<". The threshold is here to catch a cleaning
	// regression, which shows up in the thousands, not in ones.
	if limit := withProd / 10000; dirty > limit {
		t.Errorf("%d production codes contain markup, want at most %d — "+
			"cleaning has regressed", dirty, limit)
	}
	if airLost > 0 {
		t.Errorf("%d episodes lost an air date they used to have", airLost)
	}
}

// airDateFieldsOf pulls every OriginalAirDate value out of a page, so the old
// and new rules can be compared on the same inputs.
func airDateFieldsOf(text string) []string {
	var out []string
	low := strings.ToLower(text)
	for i := 0; ; {
		j := strings.Index(low[i:], "originalairdate")
		if j < 0 {
			return out
		}
		i += j + len("originalairdate")
		rest := text[i:]
		if k := strings.Index(rest, "="); k >= 0 && k < 8 {
			rest = rest[k+1:]
			if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
				rest = rest[:nl]
			}
			if v := strings.TrimSpace(rest); v != "" {
				out = append(out, v)
			}
		}
	}
}

func trunc(s string) string {
	if len(s) > 150 {
		return s[:150]
	}
	return s
}
