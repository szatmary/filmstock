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
	case "import":
		build.CmdImport(os.Args[2:])
	case "export":
		build.CmdExport(os.Args[2:])
	case "vectors":
		build.CmdVectors(os.Args[2:])
	case "catchup":
		build.CmdCatchup(os.Args[2:])
	case "sync":
		build.CmdSync(os.Args[2:])
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
	case "update":
		build.CmdUpdate(os.Args[2:])
	case "compact":
		build.CmdCompact(os.Args[2:])
	case "recompress":
		build.CmdRecompress(os.Args[2:])
	case "train-dict":
		build.CmdTrainDict(os.Args[2:])
	case "split":
		build.CmdSplit(os.Args[2:])
	case "join":
		build.CmdJoin(os.Args[2:])
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

The record tree is a separate repository: github.com/szatmary/filmstock-data
The index is derived from it and rebuilds in about two minutes.

Usage:
  filmstock build-wd-edges -db wikidata.db < ENTITIES.json     P179/P4908 edges (stdin)
  filmstock build-qidmap   -pageprops F -index IDX -db DB      title/page_id -> Q-id
  filmstock import -dumps DUMPS -inter intermediate.db          dumps -> the intermediate (pass 1)
  filmstock export -inter intermediate.db -out RECORDS         the intermediate -> records (pass 2)
  filmstock extract -dumps DUMPS -out RECORDS -cache wikidata.db
                                                               dumps -> the record tree
  filmstock extract ... -text DIR                              put the corpus elsewhere
  filmstock extract ... -index=false                           records only
  filmstock sync                                               clone/pull the store, reindex if stale
  filmstock catchup -records filmstock-data                    apply every daily since the store's last
  filmstock index   -records filmstock-data -db index.db       record store -> the index
  filmstock update  -incr DUMP -records filmstock-data         apply one day of changes
  filmstock update ... -commit [-message MSG]                   apply, report the diff, commit it
  filmstock index-television / index-events                    reindex one kind
  filmstock search  [-n 20] QUERY...                           fuzzy-search the index
  filmstock train-dict -records filmstock-data                        retrain the compression dictionaries
  filmstock compact -records filmstock-data                           reclaim space from superseded records
  filmstock recompress -in OLD -out NEW -old-dict D                   rewrite a store against new dictionaries
  filmstock split -db index.db -out index-parts/                     index -> committable parts
  filmstock join  -in index-parts/ -db index.db                      parts -> index (verified)

The browser is a separate binary:
  filmstock-web -db index.db -records filmstock-data`)
	os.Exit(2)
}
