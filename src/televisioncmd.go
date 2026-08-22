package main

import (
	"compress/gzip"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// televisionMsg is a unit of television data emitted by parseTelevisionPage: either a series record or a
// batch of episodes tagged with the page_id of the article they came from.
//
// Episodes are deliberately NOT tagged with a series title. A title with its
// disambiguator stripped cannot tell "Four on the Floor (Canadian TV series)"
// from "(American TV program)", so the season->series edge is resolved later
// from Wikidata's stated P179 ("part of the series") relation rather than being
// derived from strings.
type televisionMsg struct {
	series *TelevisionSeries

	srcID      int    // page_id of the article these episodes came from
	srcTitle   string // diagnostics only — never a key
	seriesID   int    // series page_id when the source article IS the series
	season     int
	eps        []*Episode
	seasonMeta *Season
	auth       bool // episodes from a dedicated season article
}

// parseTelevisionPage extracts series and/or episode data from one article.
func parseTelevisionPage(p Page) []televisionMsg {
	text := p.Text
	lower := strings.ToLower(text)
	var msgs []televisionMsg

	// Season article: episodes all belong to the season named in the title.
	if _, season, ok := articleSeason(p.Title); ok &&
		(strings.Contains(lower, "{{infobox television season") || strings.Contains(lower, "{{episode list")) {
		var meta *Season
		if body, ok := findTemplate(text, "Infobox television season"); ok {
			ib := parseInfobox(body)
			meta = &Season{Season: season}
			if d := parseReleaseDates(ib["first_aired"]); len(d) > 0 {
				meta.FirstAired = d[0]
			}
			if d := parseReleaseDates(ib["last_aired"]); len(d) > 0 {
				meta.LastAired = d[0]
			}
		}
		var eps []*Episode
		for _, body := range findAllTemplates(text, "Episode list") {
			if e := parseEpisodeRow(parseInfobox(body)); e != nil {
				eps = append(eps, e)
			}
		}
		if len(eps) > 0 || meta != nil {
			// The season article names its season but never its series, so the
			// owner is left blank here for Wikidata to supply.
			msgs = append(msgs, televisionMsg{srcID: p.ID, srcTitle: p.Title, season: season,
				eps: eps, seasonMeta: meta, auth: true})
		}
		return msgs
	}

	// Series article — exact match so "…episode"/"…season" infoboxes don't leak in.
	if body, ok := findTemplateExact(text, "Infobox television"); ok {
		s := buildTelevisionSeries(p.Title, p.ID, parseInfobox(body))
		s.Overview, s.Plot = extractLeadAndPlot(text)
		msgs = append(msgs, televisionMsg{series: s})
		// Episodes listed inside the series article belong to that series by
		// construction — no resolution needed.
		msgs = append(msgs, episodesByHeading(text, p.ID, p.Title, p.ID)...)
		return msgs
	}

	// "List of X episodes" article. Its owner is not stated here either.
	if reListEpisodes.MatchString(p.Title) {
		msgs = append(msgs, episodesByHeading(text, p.ID, p.Title, 0)...)
	}
	return msgs
}

// episodesByHeading groups an article's inline episodes into per-season messages,
// tagged with the source article's page_id (and its series, when self-evident).
func episodesByHeading(text string, srcID int, srcTitle string, seriesID int) []televisionMsg {
	bySeason := map[int][]*Episode{}
	for _, te := range extractEpisodesByHeading(text) {
		s := te.season
		if s == 0 {
			s = 1
		}
		bySeason[s] = append(bySeason[s], te.ep)
	}
	var msgs []televisionMsg
	for s, list := range bySeason {
		msgs = append(msgs, televisionMsg{srcID: srcID, srcTitle: srcTitle, seriesID: seriesID,
			season: s, eps: list})
	}
	return msgs
}

// televisionCollector merges television messages into series with nested seasons/episodes.
// Episodes from season articles (auth) take priority over inline list-article
// episodes, which are only used for seasons with no authoritative data.
type televisionCollector struct {
	// Identity is the page_id, never the title. Keying by a
	// disambiguator-stripped title silently dropped every show that collided
	// with another (the BBC's House of Cards, PBS Frontline, all non-US Big
	// Brothers), because the loser was simply overwritten.
	series map[int]*TelevisionSeries

	// Episodes are staged by SOURCE article page_id until the owning series is
	// known. auth = from a dedicated season article, which outranks inline
	// episodes scraped from a "List of X episodes" page.
	auth   map[int]map[int][]*Episode
	inline map[int]map[int][]*Episode
	smeta  map[int]map[int]*Season

	srcTitle map[int]string // page_id -> title, for reporting unresolved sources
	srcOwner map[int]int    // page_id -> series page_id, when the source states it
}

func newTelevisionCollector() *televisionCollector {
	return &televisionCollector{
		series:   map[int]*TelevisionSeries{},
		auth:     map[int]map[int][]*Episode{},
		inline:   map[int]map[int][]*Episode{},
		smeta:    map[int]map[int]*Season{},
		srcTitle: map[int]string{},
		srcOwner: map[int]int{},
	}
}

func addEps(m map[int]map[int][]*Episode, key int, season int, eps []*Episode) {
	sm := m[key]
	if sm == nil {
		sm = map[int][]*Episode{}
		m[key] = sm
	}
	sm[season] = append(sm[season], eps...)
}

func (c *televisionCollector) add(m televisionMsg) {
	if m.series != nil {
		id := m.series.PageID
		if old, ok := c.series[id]; !ok || len(m.series.Raw) > len(old.Raw) {
			c.series[id] = m.series
		}
	}
	if m.srcID != 0 && (len(m.eps) > 0 || m.seasonMeta != nil) {
		if m.auth {
			addEps(c.auth, m.srcID, m.season, m.eps)
		} else {
			addEps(c.inline, m.srcID, m.season, m.eps)
		}
		if m.seasonMeta != nil {
			mm := c.smeta[m.srcID]
			if mm == nil {
				mm = map[int]*Season{}
				c.smeta[m.srcID] = mm
			}
			mm[m.season] = m.seasonMeta
		}
		c.srcTitle[m.srcID] = m.srcTitle
		if m.seriesID != 0 {
			c.srcOwner[m.srcID] = m.seriesID
		}
	}
}

// televisionStats reports how the season->series resolution went, so a drop in coverage
// is visible in the run output instead of silently shrinking the data.
type televisionStats struct {
	Series      int
	Episodes    int
	Resolved    int      // episode sources attached to a series
	Unresolved  int      // episode sources with no stated owner
	OrphanNames []string // a sample of unresolved source titles
}

// finish assembles final series records, attaching sorted, de-duplicated seasons.
//
// seasonOf maps an episode-source article page_id to its series page_id, built
// from Wikidata's stated "part of the series" (P179) relation. A source with no stated
// owner is left UNATTACHED and counted — never guessed onto a same-titled show,
// which is exactly how the two Hawaii Five-Os and fifty Big Brothers would merge.
// listOwner maps a "List of X episodes" article page_id to its series page_id,
// built by inverting the series' own list_episodes infobox link.
func (c *televisionCollector) finish(seasonOf, listOwner map[int]int) ([]*TelevisionSeries, televisionStats) {
	var st televisionStats

	// Map each episode source to its owning series.
	sources := map[int]bool{}
	for id := range c.auth {
		sources[id] = true
	}
	for id := range c.inline {
		sources[id] = true
	}
	for id := range c.smeta {
		sources[id] = true
	}
	bySeries := map[int][]int{}
	for src := range sources {
		owner, ok := c.srcOwner[src]
		if !ok {
			owner, ok = seasonOf[src]
		}
		if !ok {
			// The series states where its episode list lives; inverting that
			// link is as authoritative as P179, just authored on enwiki.
			owner, ok = listOwner[src]
		}
		if ok {
			if _, known := c.series[owner]; known {
				bySeries[owner] = append(bySeries[owner], src)
				st.Resolved++
				continue
			}
		}
		st.Unresolved++
		if len(st.OrphanNames) < 25 {
			st.OrphanNames = append(st.OrphanNames, c.srcTitle[src])
		}
	}

	var out []*TelevisionSeries
	for id, s := range c.series {
		srcs := bySeries[id]
		// Union of season numbers from authoritative and inline sources.
		seasons := map[int][]*Episode{}
		for _, src := range srcs {
			for snum, list := range c.inline[src] {
				seasons[snum] = append(seasons[snum], list...)
			}
		}
		authSeasons := map[int][]*Episode{}
		for _, src := range srcs {
			for snum, list := range c.auth[src] {
				authSeasons[snum] = append(authSeasons[snum], list...)
			}
		}
		for snum, list := range authSeasons {
			seasons[snum] = list // authoritative overrides inline for this season
		}
		for snum, list := range seasons {
			seen := map[string]bool{}
			var ded []*Episode
			for _, e := range list {
				key := "t|" + e.Title
				if e.NumberInSeason > 0 {
					key = "n|" + strconv.Itoa(e.NumberInSeason)
				}
				if seen[key] {
					continue
				}
				seen[key] = true
				ded = append(ded, e)
			}
			sort.SliceStable(ded, func(i, j int) bool {
				if ded[i].NumberInSeason != ded[j].NumberInSeason {
					return ded[i].NumberInSeason < ded[j].NumberInSeason
				}
				return ded[i].NumberOverall < ded[j].NumberOverall
			})
			sea := &Season{Season: snum, Episodes: ded, NumEpisodes: len(ded)}
			for _, src := range srcs {
				if mm := c.smeta[src]; mm != nil {
					if meta, ok := mm[snum]; ok {
						sea.FirstAired, sea.LastAired = meta.FirstAired, meta.LastAired
						break
					}
				}
			}
			s.Seasons = append(s.Seasons, sea)
			st.Episodes += len(ded)
		}
		sort.SliceStable(s.Seasons, func(i, j int) bool { return s.Seasons[i].Season < s.Seasons[j].Season })
		out = append(out, s)
	}
	st.Series = len(out)
	return out, st
}

var reTelevisionSlug = reSlug

// writeTelevisionSeries writes one gzip-compressed series JSON, sharded like movies.
func writeTelevisionSeries(outDir string, s *TelevisionSeries) error {
	sum := md5.Sum([]byte(s.Title))
	dir := filepath.Join(outDir, hex.EncodeToString(sum[:1]))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	slug := reTelevisionSlug.ReplaceAllString(s.Title, "_")
	if len(slug) > 80 {
		slug = slug[:80]
	}
	fname := fmt.Sprintf("%s-%d.json.gz", slug, s.PageID)
	f, err := os.Create(filepath.Join(dir, fname))
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	enc := json.NewEncoder(gz)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

// cmdTelevisionTest assembles television from local wikitext files to validate the pipeline.
// Usage: filmstock television-test title=file [title=file ...]
func cmdTelevisionTest(args []string) {
	coll := newTelevisionCollector()
	var pid int
	for _, a := range args {
		i := strings.Index(a, "=")
		if i < 0 {
			continue
		}
		title, path := a[:i], a[i+1:]
		data, err := os.ReadFile(path)
		if err != nil {
			fatal(err)
		}
		pid++
		// mimic xml.Decoder entity unescaping that the streaming parser gets
		p := Page{Title: title, NS: 0, ID: pid, Text: html.UnescapeString(string(data))}
		for _, m := range parseTelevisionPage(p) {
			coll.add(m)
		}
	}
	series, _ := coll.finish(nil, nil)
	for _, s := range series {
		s.Raw = nil
		out, _ := json.MarshalIndent(s, "", "  ")
		fmt.Println(string(out))
	}
}

// cmdTelevisionDebug parses a single wikitext file and prints what the television parser sees.
// Usage: filmstock television-debug <wikitext-file> [title]
func cmdTelevisionDebug(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: filmstock television-debug <wikitext-file> [title]")
		os.Exit(2)
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		fatal(err)
	}
	text := string(data)
	title := "Sample"
	if len(args) > 1 {
		title = args[1]
	}

	if body, ok := findTemplate(text, "Infobox television season"); ok {
		ib := parseInfobox(body)
		fmt.Printf("SEASON ARTICLE: season_number=%s first_aired=%s\n",
			ib["season_number"], firstNonEmpty(ib["first_aired"]))
	} else if body, ok := findTemplate(text, "Infobox television"); ok {
		s := buildTelevisionSeries(title, 0, parseInfobox(body))
		s.Raw = nil
		out, _ := json.MarshalIndent(s, "", "  ")
		fmt.Println("SERIES:")
		fmt.Println(string(out))
	}

	if series, season, ok := articleSeason(title); ok {
		fmt.Printf("TITLE parses as season: series=%q season=%d\n", series, season)
	}

	eps := extractEpisodesByHeading(text)
	fmt.Printf("\nEPISODES found: %d\n", len(eps))
	for i, te := range eps {
		if i >= 6 {
			fmt.Println("  ...")
			break
		}
		fmt.Printf("  [s%d] #%d.%d %q air=%s dir=%v writ=%v\n",
			te.season, te.ep.NumberOverall, te.ep.NumberInSeason, te.ep.Title,
			te.ep.AirDate, te.ep.DirectedBy, te.ep.WrittenBy)
	}
}
