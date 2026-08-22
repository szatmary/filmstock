package main

import (
	"fmt"
	"os"

	"github.com/szatmary/filmstock/internal/build"
)

// Page mirrors the fields we need from each <page> in the dump.
func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "index":
		build.CIndexRecords(os.Args[2:])
	case "index-movies":
		build.CIndex(os.Args[2:])
	case "search":
		build.CSearch(os.Args[2:])
	case "television-debug":
		build.CTelevisionDebug(os.Args[2:])
	case "index-events":
		build.CIndexEvents(os.Args[2:])
	case "index-television":
		build.CIndexTelevision(os.Args[2:])
	case "build-qidmap":
		build.CBuildQidmap(os.Args[2:])
	case "pack":
		build.CPack(os.Args[2:])
	case "extract":
		build.CExtract(os.Args[2:])
	case "build-wd-edges":
		build.CBuildWDEdges(os.Args[2:])
	case "television-test":
		build.CTelevisionTest(os.Args[2:])
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
  filmstock pack    -records OUTDIR                                   records -> packs + offsets

The browser is a separate binary:
  filmstock-web -db OUTDIR/search.db -records OUTDIR
  filmstock-web -db OUTDIR/search.db -remote https://.../v2026-08-22

Retrieval experiments (ColBERT, dense vectors, quantisation, the eval harness)
live on the ai-experiments branch.`)
	os.Exit(2)
}
