# filmstock

A media database built from the English Wikipedia and Wikidata dumps: films, television
series and episodes, award ceremonies and festivals, and the people credited on
them — parsed, cross-linked by Wikidata Q-id, and indexed for search.

No API keys, no scraping, no third-party service. One pass over the dumps
produces a self-contained record hierarchy and a SQLite database you can serve.

**Current build** (enwiki 2026-07-07 dump):

| | rows in `index.db` |
|---|---|
| films | 165,265 |
| award ceremonies & festivals | 4,669 |
| television series | 61,137 |
| television episodes | 551,202 |
| people | 219,050 (148,030 with a Q-id, 67.6%) |
| credits | 1,294,290 |

Records written to disk differ slightly from rows indexed — 219,628 people
records against 219,050 rows, since a person needs a credit to be indexed. Quote
whichever you mean; the two are not interchangeable.

Extract is **7,459 s CPU / 1.9 GB peak RSS**, and 65.5 min wall on a 5-disk USB
raidz1 array that was also serving other reads at the time. An earlier run on an
idle box measured 48.4 min wall for 7,103 s CPU — the CPU figure is the stable
one, and the gap between them is the point: this is I/O bound, not CPU bound. On
NVMe it is substantially faster.

Indexing from records already on disk is **2 min 20 s** (`make index`), no dump
read.

## Why the identity work matters

The hard part is not parsing infoboxes, it is deciding *what is the same thing*.

- Every record is keyed on `page_id` or Wikidata Q-id — never on a display
  string. 50 different series render as "Big Brother"; keying on the title
  merges them. The same mistake merged distinct people who happened to share a
  name.
- `{{Infobox film awards}}` and `{{Infobox Film festival}}` both start with
  `Infobox film`. Prefix-matching them put 2,418 award ceremonies and 1,647
  festivals into the film index with year 0 and no cast. Templates are matched
  exactly, with a regression test.
- Season → series edges come from Wikidata's stated `P179`/`P4908`, not from
  title similarity. When no edge is stated the episode source is left
  unattached and **counted** (1,705 today, mostly anime seasons) rather than
  guessed at.

## Build it

You need Go 1.25+, `sqlite3`, and `lbzip2` (or `bzip2`). Budget ~130 GB for the
dumps, ~60 GB for the outputs, and ~28 GB for the build-time resolver cache.

```sh
make build          # -> ./filmstock
```

### 1. Fetch the dumps

```sh
make dumps          # ~129 GB into ./dump — go do something else
```

Four files, from `dumps.wikimedia.org`:

| file | size | what it is |
|---|---|---|
| `enwiki-latest-pages-articles-multistream.xml.bz2` | 26.5 GB | every article |
| `enwiki-latest-pages-articles-multistream-index.txt.bz2` | 283 MB | byte offsets, for parallel reads |
| `enwiki-latest-page_props.sql.gz` | 457 MB | page_id → Q-id |
| `latest-all.json.bz2` (wikidata) | 102 GB | the entity graph |

### 2. Build the resolver cache

Both steps write `wikidata.db`, so they must not overlap. This is build-time
only and discardable once extraction has run.

```sh
make resolver       # build-wd-edges (P179/P4908 edges) then build-qidmap
```

`extract` **refuses to run** without the season→series edge table rather than
falling back to title matching.

### 3. Extract and index

```sh
make extract        # dumps -> the record store + the index (one pass, ~43 min)
```

One linear pass over the 26.5 GB dump handles films and television together. It is not
incremental and never has been — incremental ingest would mean consuming
Wikimedia's daily `adds-changes` dumps instead, which is a different pipeline.

To rebuild only the database from records already on disk:

```sh
make index          # out/ -> out/index.db, no dump read
```

### 4. Serve it

```sh
make serve          # http://localhost:8080
```

## Layout

Three things sit side by side. Only the first is this repository.

```
filmstock/          this repo — the library, the CLI, the browser
filmstock-data/     the record store, its own repo (450,701 records, 376 MB)
dump/               the Wikimedia dumps, ~129 GB of input
build/              everything derived: index.db, the corpus, vectors
wikidata.db         build-time resolver cache, discardable
```

Inside this repo:

```
github.com/szatmary/filmstock        the library — import this
  db.go              Open, DB, the search methods, ErrNotFound
  sync.go            SyncStore: clone/update the filmstock-data checkout (consumers)
  search*.go         lexical search: FTS5 trigram + fuzzy ranking
  record.go          RecordSource: Dir, and the readers
  paths.go           record paths, kind constants, tree walking
  movie.go television.go event.go people.go     record types
internal/
  wikitext/          template and infobox parsing, link/ref cleanup
  dump/              multistream bz2 reading off the offset index
  build/             dumps -> records -> index.db, resolvers, splitting
cmd/
  filmstock/         the CLI — dispatch only
  filmstock-web/     the browser
```

Implementation lives under `internal/` rather than inside a `package main`, so it
is importable and unit-testable. The library sits at the module root because that
is what makes the import path `github.com/szatmary/filmstock` rather than
`.../pkg/filmstock`.

The Makefile's paths point outward (`../dump`, `../filmstock-data`, `../build`)
and every one is overridable. The tools' own flag defaults assume the simpler
case the README documents: a working directory holding `filmstock-data/` and
`index.db`.

## Records are byte-deterministic

Identical input produces identical bytes — Go's gzip writes no timestamp and
`encoding/json` sorts map keys. That is the precondition for distributing the
data as diffs between ingests rather than as a full re-download, which is where
this is headed.

Ingest is currently additive-only: a page that disappears from the dump is not
removed from a previous build. Deletion handling comes with the diff work.

## Scope

`main` is the Wikipedia processing and a browser over the result: parse the
dumps, resolve identity against Wikidata, index for lookup, serve it. Search here
is lexical — FTS5 trigram over titles, cast and creators — which is what finding
a specific entry needs.

Semantic search is **not** part of main. Dense embeddings, ColBERT late
interaction, quantisation, cross-encoder reranking, RRF fusion, the Python
embedding jobs and the eval harness all live on the **`ai-experiments`** branch,
along with `docs/embeddings.md` and the measurements that settled which of them
earned their keep.

## Known rough edges

- The plain-text corpus is films only. 61k series and 551k episodes get no
  `out/text/` entry, so anything built on that text covers films alone.

## Use it as a library

```go
import "github.com/szatmary/filmstock"

db, err := filmstock.Open("index.db", filmstock.Remote(baseURL))
defer db.Close()

films, _ := db.SearchFilms(ctx, "blade runner", "title", 20)  // database only
film,  _ := db.Film(ctx, films[0].ID)                          // one range request
fmt.Println(film.Plot, film.Cinematography, film.RawInfobox)
```

Every search, ranking and count is answered from `index.db` alone and touches no
network. Only `Film`, `Series` and `Event` reach the record source. That split is
the reason a 161 MB download can still open 620k full records.

Where records come from is one argument:

```go
filmstock.Store("filmstock-data")   // the record stores, one per kind
```

`modernc.org/sqlite` is used rather than a cgo driver, so importing this package
does not force cgo on you.

### The sample app

`cmd/filmstock-web` is a small web server built on nothing but the exported API —
no internals, no SQL of its own. If a working server could not be written against
the public surface, the surface would be wrong, so it is a test as much as a demo.

```sh
make web                                              # local records
./filmstock-web -db out/index.db -remote https://…   # remote packs
```

It refuses to guess between the two, and returns an `X-Record-Fetch` header so
the cost of each is visible. Measured on this machine: **2.9 ms** local versus
**4.8 ms** over HTTP, returning byte-identical records.

## Where the data lives

The records are a separate repository:
**[github.com/szatmary/filmstock-data](https://github.com/szatmary/filmstock-data)**
— 450,701 records, 376 MB, committed.

Separate because the Go module proxy serves a zip of the entire module tree and
caps it at 500 MiB, so records in this module root would make every `go get` of
the library pay for them.

```sh
git clone https://github.com/szatmary/filmstock-data
filmstock index -records filmstock-data -db index.db     # ~3 minutes, once
filmstock-web  -db index.db -records filmstock-data
```

Records live in [gitdb](https://github.com/szatmary/gitdb) stores — one record
per line of text, zlib-compressed against a per-kind dictionary and Base94
encoded — so git diffs them by line and changing one record is one changed line.
The dictionary is what makes that affordable: it buys 21.7% over plain zlib,
very nearly cancelling Base94's 25% expansion.

Measured, on this corpus:

| | |
|---|---|
| store | 376 MB working tree, 361.53 MiB packed, 117 files |
| largest file | 4.00 MB — GitHub rejects at 100 MiB |
| full pass | 43.1 min (parse ~40, index 3.0) |
| one day of changes | `filmstock update`, 0.8 s |

The index is in neither repository. It is derived, it rebuilds in about three
minutes, and a rebuild changes 100% of its bytes — so committing it would cost
hundreds of megabytes of permanent history per ingest for something regenerable.
`filmstock index` stamps the store state it was built from, and the tools warn if
you pull new records without reindexing.

## Data license

The records are derived from English Wikipedia and Wikidata. Wikipedia text is
**CC BY-SA 4.0**; Wikidata is **CC0**. Anything redistributed from `out/`
carries those terms and needs the attribution.
