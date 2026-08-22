package main

import (
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

// Chunking the embedding corpus into overlapping passages.
//
// Embedding models accept only a few hundred tokens, but articles run to
// thousands of words, so each article becomes several passages and a work ends up
// with several vectors. That is deliberate rather than a workaround: it is what
// lets "1978 box office flops" match the Box office passage specifically instead
// of being diluted by the whole article. Whole-article embeddings average away
// exactly the signal these queries depend on.
//
// Windows are measured in WORDS, not tokens, because the tokenizer belongs to a
// model we have not chosen yet. English subword tokenizers run about 1.3 tokens
// per word, so the defaults below (246 words, 49 overlap) target ~320 tokens with
// ~64 overlap. Both are flags: once a model is picked, re-chunking is cheap —
// it is a local pass over text we already have, not another dump read.

// Passage is one embeddable window. Start is a byte offset into the source text
// so a hit can be traced back to the exact prose that produced it.
type Passage struct {
	PageID  int    `json:"page_id"`
	Ord     int    `json:"ord"`
	Start   int    `json:"start"`
	Section string `json:"section,omitempty"`
	Text    string `json:"text"`
}

// Corpus profiles — SMALL / MEDIUM / LARGE.
//
// The same records produce different passage sets for different deployment
// targets. The build pipeline is shared; only what gets embedded diverges, so a
// single extract feeds every target.
//
// Measured section shares (2000-article sample): Plot 23.4%, lead 10.6%,
// External links 10.2%, Cast 6.7%, Production 4.5%, Reception 4.3%,
// Critical response 2.8%, Accolades 2.2%, Box office 0.9%.
//
//	profile  keeps                                  ~corpus  int2@1024d  int2@256d
//	small    lead + plot                              ~34%      47 MB      12 MB
//	medium   small + cast + reception + box office    ~49%      68 MB      17 MB
//	large    everything except apparatus              ~83%     115 MB      29 MB
//
// small  — browser / WASM. The 35 MB download budget only closes at 256 dims,
//          and plot is what concept queries actually match.
// medium — Raspberry Pi 4 / N100. Adds cast (identity queries) and reception
//          (the "box office flops" class). The default.
// large  — BIG IRON. Server-side, no device budget: keeps production,
//          development, marketing and awards, which support questions about how
//          films were made. This is the profile to pair with the expensive
//          machinery that cannot run on a device — ColBERT late interaction
//          (21.7 GB at full corpus, 2.3 GB at small), cross-encoder reranking
//          (measured 41-219 ms/pair, i.e. 7-35 s/query on a Pi), or a 7B
//          embedder at 4096 dims. Those are all viable on a server and none are
//          viable on a Pi, so they belong to one profile, not to all of them.

// apparatus is dropped by every profile: reference lists and navigation with no
// retrieval value. 10.2% of the corpus is External links alone.
var apparatus = map[string]bool{
	"external links": true, "references": true, "see also": true,
	"notes": true, "bibliography": true, "further reading": true,
	"sources": true, "citations": true, "footnotes": true,
	"external link": true, "reference": true,
}

var smallKeep = map[string]bool{
	"": true, "plot": true, "synopsis": true, "premise": true, "story": true,
	"plot summary": true, "storyline": true, "summary": true,
}

// medium adds the sections that answer identity and reception questions.
var mediumExtra = map[string]bool{
	"cast": true, "cast and characters": true, "characters": true,
	"reception": true, "critical reception": true, "critical response": true,
	"box office": true, "themes": true, "analysis": true,
	"accolades": true, "awards": true,
}

// keepSection decides whether a section is embedded under the given profile.
// Unknown headings are kept by `large` (denylist) and dropped by small/medium
// (allowlist) — so an unusual heading costs storage rather than being silently
// lost from the big-iron index.
func keepSection(profile, name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if apparatus[n] {
		return false
	}
	switch profile {
	case "small":
		return smallKeep[n]
	case "medium", "lean":
		return smallKeep[n] || mediumExtra[n]
	case "all":
		return true
	default: // large, full
		return true
	}
}

// reSection matches a wikitext section heading, which cleanText preserves.
// Present in 300/300 sampled articles, median 5 per article.
var skipped int64

var reSection = regexp.MustCompile(`(?m)^\s*={2,}\s*([^=\n]{1,60}?)\s*={2,}\s*$`)

// splitSections cuts an article at its headings so a passage never spans two
// topics. A window that straddles Plot and Production is about neither, which is
// how "1978 box office flops" ends up matching prose about casting.
func splitSections(s string) []struct{ Name, Body string } {
	var out []struct{ Name, Body string }
	locs := reSection.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return []struct{ Name, Body string }{{"", s}}
	}
	if locs[0][0] > 0 {
		out = append(out, struct{ Name, Body string }{"", s[:locs[0][0]]})
	}
	for i, m := range locs {
		name := strings.TrimSpace(s[m[2]:m[3]])
		start := m[1]
		end := len(s)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		body := s[start:end]
		if strings.TrimSpace(body) != "" {
			out = append(out, struct{ Name, Body string }{name, body})
		}
	}
	return out
}

// buildHeader is the contextual prefix given to every passage of a film.
//
// Without it, most passages are orphaned prose: 29 of The Martian's 30 windows
// never mention the film, the year, or Matt Damon, so a query naming any of them
// can only match the one passage that happens to. Repeating a short identity line
// makes every passage answerable on identity AND content at once, which is what
// "matt damon growing potatoes" needs — those two facts live 12 passages apart.
func buildHeader(m *Movie, section string) string {
	var b strings.Builder
	b.WriteString(cleanTitle(m.Title))
	if y := filmYear(m); y != "" {
		b.WriteString(" (" + y + ")")
	}
	if len(m.Director) > 0 {
		b.WriteString(", directed by " + m.Director[0].Name)
	}
	if len(m.Starring) > 0 {
		names := make([]string, 0, 3)
		for i, p := range m.Starring {
			if i == 3 {
				break
			}
			names = append(names, p.Name)
		}
		b.WriteString(", starring " + strings.Join(names, ", "))
	}
	b.WriteString(".")
	if section != "" {
		b.WriteString(" " + section + ".")
	}
	return b.String() + " "
}

// filmYear takes the first 4-digit year from the release dates.
func filmYear(m *Movie) string {
	for _, d := range m.ReleaseDates {
		if len(d) >= 4 {
			return d[:4]
		}
	}
	return ""
}

// wordSpan is one word's byte range in the source.
type wordSpan struct{ lo, hi int }

// splitWords records byte offsets rather than returning strings, so passages can
// be sliced out of the original text and keep an exact provenance offset.
func splitWords(s string) []wordSpan {
	var out []wordSpan
	inWord := false
	start := 0
	for i, r := range s {
		sep := unicode.IsSpace(r)
		if !sep && !inWord {
			start, inWord = i, true
		} else if sep && inWord {
			out = append(out, wordSpan{start, i})
			inWord = false
		}
	}
	if inWord {
		out = append(out, wordSpan{start, len(s)})
	}
	return out
}

// chunkText splits s into overlapping windows of `window` words, advancing by
// (window - overlap) each step.
func chunkText(pageID int, s string, window, overlap int) []Passage {
	if window <= 0 {
		window = 246
	}
	if overlap < 0 || overlap >= window {
		overlap = window / 5
	}
	words := splitWords(s)
	if len(words) == 0 {
		return nil
	}
	stride := window - overlap
	var out []Passage
	for i, ord := 0, 0; i < len(words); i, ord = i+stride, ord+1 {
		end := i + window
		if end > len(words) {
			end = len(words)
		}
		lo, hi := words[i].lo, words[end-1].hi
		out = append(out, Passage{PageID: pageID, Ord: ord, Start: lo, Text: s[lo:hi]})
		if end == len(words) {
			break // final window already reached the end; a shorter tail adds nothing
		}
	}
	return out
}

// pageIDFromPath recovers the page_id from out/text/<shard>/<page_id>.txt.gz.
// The path is a pure function of identity, so this is exact, not a guess.
func pageIDFromPath(p string) (int, bool) {
	base := filepath.Base(p)
	base = strings.TrimSuffix(base, ".txt.gz")
	id, err := strconv.Atoi(base)
	return id, err == nil
}

func cmdChunk(args []string) {
	fs := flag.NewFlagSet("chunk", flag.ExitOnError)
	records := fs.String("records", "/tank/mediadb/out", "record hierarchy")
	out := fs.String("out", "", "output passages (.jsonl.gz); default <records>/passages.jsonl.gz")
	window := fs.Int("window", 246, "window size in words (~320 subword tokens)")
	overlap := fs.Int("overlap", 49, "overlap in words (~64 subword tokens)")
	workers := fs.Int("workers", 12, "parallel readers")
	noHeader := fs.Bool("no-header", false, "omit the contextual header (for A/B comparison)")
	profile := fs.String("sections", "medium",
		"corpus profile: small (browser) | medium (Pi/N100) | large (server) | all")
	fs.Parse(args)

	target := *out
	if target == "" {
		target = filepath.Join(*records, "passages.jsonl.gz")
	}

	var paths []string
	if err := walkRecords(*records, kindText, func(p string) error {
		paths = append(paths, p)
		return nil
	}); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "chunking %d documents (profile=%s window=%d overlap=%d words)\n",
		len(paths), *profile, *window, *overlap)

	f, err := os.Create(target)
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()

	type batch struct {
		ps []Passage
	}
	jobs := make(chan string, 1024)
	results := make(chan batch, 1024)
	var wg sync.WaitGroup
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				id, ok := pageIDFromPath(p)
				if !ok {
					continue
				}
				rf, err := os.Open(p)
				if err != nil {
					continue
				}
				zr, err := gzip.NewReader(rf)
				if err != nil {
					rf.Close()
					continue
				}
				var sb strings.Builder
				buf := make([]byte, 32<<10)
				for {
					n, err := zr.Read(buf)
					if n > 0 {
						sb.Write(buf[:n])
					}
					if err != nil {
						break
					}
				}
				zr.Close()
				rf.Close()

				// Load the film record for the contextual header. Without it a
				// passage is orphaned prose; with it every window is answerable
				// on identity as well as content.
				var mv Movie
				hdrOK := !*noHeader &&
					readRecordJSON(recordPath(*records, kindMovie, int64(id), ".json.gz"), &mv) == nil

				var all []Passage
				ord := 0
				for _, sec := range splitSections(sb.String()) {
					if !keepSection(*profile, sec.Name) {
						atomic.AddInt64(&skipped, 1)
						continue
					}
					for _, p := range chunkText(id, sec.Body, *window, *overlap) {
						p.Section = sec.Name
						p.Ord = ord
						ord++
						if hdrOK {
							p.Text = buildHeader(&mv, sec.Name) + p.Text
						}
						all = append(all, p)
					}
				}
				results <- batch{all}
			}
		}()
	}
	go func() { wg.Wait(); close(results) }()

	go func() {
		for _, p := range paths {
			jobs <- p
		}
		close(jobs)
	}()

	enc := json.NewEncoder(gz)
	var docs, passages int64
	start := time.Now()
	for b := range results {
		for _, p := range b.ps {
			if err := enc.Encode(&p); err != nil {
				fatal(err)
			}
			passages++
		}
		if d := atomic.AddInt64(&docs, 1); d%20000 == 0 {
			fmt.Fprintf(os.Stderr, "\r  %d docs, %d passages (%.0f docs/s)",
				d, passages, float64(d)/time.Since(start).Seconds())
		}
	}
	fmt.Fprintf(os.Stderr, "  sections skipped by profile: %d\n", atomic.LoadInt64(&skipped))
	fmt.Fprintf(os.Stderr, "\rDONE: %d docs -> %d passages in %.1fs (%.1f passages/doc)\n",
		docs, passages, time.Since(start).Seconds(), float64(passages)/float64(max64(docs, 1)))
	fmt.Fprintf(os.Stderr, "  written to %s\n", target)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
