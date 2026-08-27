package main

import (
	"fmt"
	"os"

	"github.com/szatmary/filmstock/internal/build"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "import":
		build.CmdImport(os.Args[2:])
	case "export":
		build.CmdExport(os.Args[2:])
	case "compose-vectors":
		build.CmdComposeVectors(os.Args[2:])
	case "vectors":
		build.CmdVectors(os.Args[2:])
	case "catchup":
		build.CmdCatchup(os.Args[2:])
	case "search":
		build.CSearch(os.Args[2:])
	case "television-debug":
		build.CTelevisionDebug(os.Args[2:])
	case "lint":
		build.CLint(os.Args[2:])
	case "index-series":
		build.CIndexSeries(os.Args[2:])
	case "index-vectors":
		build.CIndexVectors(os.Args[2:])
	case "index-external-ids":
		build.CIndexExternalIDs(os.Args[2:])
	case "builds":
		build.CmdBuilds(os.Args[2:])
	case "manifest":
		build.CmdManifest(os.Args[2:])
	case "build-image-list":
		build.CBuildImageList(os.Args[2:])
	case "build-qidmap":
		build.CBuildQidmap(os.Args[2:])
	case "update":
		build.CmdUpdate(os.Args[2:])
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

The pipeline is two passes: import stores every recognised page in the
intermediate; export re-derives the published SQLite databases from it.

Usage:
  filmstock build-wd-edges -db wikidata.db < ENTITIES.json     P179/P4908 edges (stdin)
  filmstock build-qidmap   -pageprops F -index IDX -db DB      title/page_id -> Q-id
  filmstock build-image-list -image F -db DB                   local-file list for CDN image URLs
  filmstock import -dumps DUMPS -inter intermediate.db         dumps -> the intermediate (pass 1)
  filmstock export -inter intermediate.db -db filmstock.db     the intermediate -> the databases (pass 2)
  filmstock extract -dumps DUMPS -db filmstock.db              both passes' work in one command, dump-direct
  filmstock update  -incr DAY.xml.bz2 -db filmstock.db         one day: into the intermediate, then re-export
  filmstock catchup -db filmstock.db                           apply every daily the intermediate is behind
  filmstock index-series / index-vectors / index-external-ids  post-passes over the database
  filmstock compose-vectors / vectors                          embedding artifacts
  filmstock manifest / builds                                  release metadata (manifest.json, builds.json)
  filmstock search  [-n 20] QUERY...                           fuzzy-search the database

The browser is a separate binary:
  filmstock-web -db filmstock.db`)
	os.Exit(2)
}
