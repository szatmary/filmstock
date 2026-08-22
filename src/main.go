package main

import (
	"fmt"
	"os"
	"regexp"
)

// Page mirrors the fields we need from each <page> in the dump.
type Page struct {
	Title string `xml:"title"`
	NS    int    `xml:"ns"`
	ID    int    `xml:"id"`
	Text  string `xml:"revision>text"`
}

var reInfoboxFilm = regexp.MustCompile(`(?i)\{\{\s*infobox film(\b|\s|\||\})`)

// buildFilm returns the film record for a page, or nil if the page is not a
// film. This is the single definition of "what counts as a film and how it is
// parsed" — both the legacy writer and `extract` call it, so they cannot drift
// apart and start disagreeing about what a given page is.
func buildFilm(p Page) *Movie {
	if p.NS != 0 {
		return nil
	}
	if !reInfoboxFilm.MatchString(p.Text) {
		return nil
	}
	// EXACT, not prefix: "{{Infobox film awards}}" and "{{Infobox Film
	// festival}}" both start with "Infobox film", and matching them by prefix
	// put 2,418 awards ceremonies and 1,647 festivals into the film index with
	// year 0 and no cast. They are real media and get their own record type —
	// see event.go. This is the same collision findTemplateExact was introduced
	// to fix on the television side.
	body, ok := findTemplateExact(p.Text, "Infobox film")
	if !ok {
		return nil
	}
	m := buildMovie(p.Title, p.ID, parseInfobox(body))
	m.Overview, m.Plot = extractLeadAndPlot(p.Text)
	m.Genre = extractGenres(p.Text)
	return m
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "index":
		cmdIndexRecords(os.Args[2:])
	case "index-movies":
		cmdIndex(os.Args[2:])
	case "filmvecs":
		cmdFilmVectors(os.Args[2:])
	case "notability":
		cmdNotability(os.Args[2:])
	case "colbert-quantize":
		cmdColbertQuantize(os.Args[2:])
	case "quantize":
		cmdQuantize(os.Args[2:])
	case "chunk":
		cmdChunk(os.Args[2:])
	case "eval-vec":
		cmdEvalVector(os.Args[2:])
	case "eval-colbert":
		cmdEvalColbert(os.Args[2:])
	case "eval":
		cmdEval(os.Args[2:])
	case "search":
		cmdSearch(os.Args[2:])
	case "serve":
		cmdServe(os.Args[2:])
	case "television-debug":
		cmdTelevisionDebug(os.Args[2:])
	case "index-events":
		cmdIndexEvents(os.Args[2:])
	case "index-television":
		cmdIndexTelevision(os.Args[2:])
	case "build-qidmap":
		cmdBuildQidmap(os.Args[2:])
	case "extract":
		cmdExtract(os.Args[2:])
	case "build-wd-edges":
		cmdBuildWDEdges(os.Args[2:])
	case "television-test":
		cmdTelevisionTest(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `filmstock — a media database built from the Wikipedia and Wikidata dumps

Usage:
  filmstock build-wd-edges -db wikidata.db < ENTITIES.json            P179/P4908 edges (stdin)
  filmstock build-qidmap   -pageprops F -index IDX -db wikidata.db    title/page_id -> Q-id
  filmstock extract -dumps DUMPDIR -out OUTDIR -cache wikidata.db     dumps -> records + search.db
  filmstock extract ... -index=false                                  records only, skip the index
  filmstock index   -records OUTDIR                                   rebuild search.db from the records alone
  filmstock index-television -television OUTDIR/television -db DB     reindex television only
  filmstock index-events     -events OUTDIR/events -db DB             reindex events only
  filmstock search [-n 20] QUERY...                                   fuzzy-search the index
  filmstock eval   [-v] [-n 20]                                       score retrieval against docs/eval/queries.json
  filmstock serve  -db OUTDIR/search.db -movies OUTDIR/movies \
                   -television OUTDIR/television -events OUTDIR/events [-addr :8080]`)
	os.Exit(2)
}

var reSlug = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "fatal:", err)
	os.Exit(1)
}
