# filmstock

A media database built from the English Wikipedia and Wikidata dumps: films, television
series and episodes, award ceremonies and festivals, and the people credited on
them — parsed, cross-linked by Wikidata Q-id, and indexed for search.

No API keys, no scraping, no third-party service. One pass over the dumps
produces a self-contained record hierarchy and a SQLite database you can serve.

**Current build** (enwiki 2026-07-07 dump):

| | rows in `search.db` |
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
make extract        # dumps -> out/{movies,television,people,events,text}/ + out/search.db
```

One linear pass over the 26.5 GB dump handles films and television together. It is not
incremental and never has been — incremental ingest would mean consuming
Wikimedia's daily `adds-changes` dumps instead, which is a different pipeline.

To rebuild only the database from records already on disk:

```sh
make index          # out/ -> out/search.db, no dump read
```

### 4. Serve it

```sh
make serve          # http://localhost:8080
```

## Layout

```
src/                Go module (module filmstock) — all the tools, one binary
  extract.go          dumps -> records; the single pass
  multistream.go      parallel bz2 stream reads off the index
  wikitext.go         template/infobox parsing, link and ref cleanup
  movie.go            film record construction
  television.go       series, seasons and episodes
  event.go            award ceremonies and festivals
  people.go           credits and person identity
  televisionresolve.go  season -> series via stated Wikidata edges
  qidmap.go wdedges.go  the resolver cache builders
  index*.go           records -> search.db (FTS5)
  serve.go browse.go  the web UI
  templates/          HTML
docs/
  TODO.md             what is done, what is open, what was settled by measurement
  embeddings.md       semantic search design
  eval/               the 415-query evaluation set
```

## Records are byte-deterministic

Identical input produces identical bytes — Go's gzip writes no timestamp and
`encoding/json` sorts map keys. That is the precondition for distributing the
data as diffs between ingests rather than as a full re-download, which is where
this is headed.

Ingest is currently additive-only: a page that disappears from the dump is not
removed from a previous build. Deletion handling comes with the diff work.

## The semantic search work

Retrieval experiments — dense embeddings, ColBERT late interaction,
quantisation, cross-encoder reranking, RRF fusion — live on the
**`ai-experiments`** branch, along with the Python embedding jobs. `main` is the
build pipeline.

The Go code for those paths ships on `main` too (they are opt-in `serve` flags,
off by default) so the two branches never diverge in a shared file. What the
branch adds is `embed/`, the sweep scripts, and the measurement logs.

Measured results are in [docs/TODO.md](docs/TODO.md) — including the ones that
say *don't do this*: equal-weight RRF fusion scores below its better input, a
cross-encoder third stage lost to not reranking, and a bigger ColBERT encoder
lost to a smaller one.

## Known rough edges

- The passage corpus is films only. 61k series and 551k episodes have no
  embedding text yet.

## Distributing it

The intended consumer download is `search.db` alone, with the per-record detail
fetched on demand by HTTP range request out of a single packed blob — not 620k
loose files. Design and the reasoning in [docs/TODO.md](docs/TODO.md) §D1. The
fetch side is not built yet; `serve` reads records from a local directory today.

The database is sized for that role: it stores **no prose**. FTS covers titles,
cast and creators only, so episode summaries and series overview/plot were
unindexed text duplicating what the record `.json.gz` already holds. They are not
stored, and `movies` never had them.

```sh
make dist           # -> out/search.db.zst
```

Measured end to end:

| | size |
|---|---|
| with prose | 637 MB |
| without prose | **383 MB** |
| `search.db.zst` (zstd -19) | **161 MB** |

`television_episodes` went from 209 MB to 26.8 MB and `television_series` from
106 MB to 17.3 MB. zstd -19 beat gzip -6 by 29% on these pages (2.79x vs 2.16x
before the trim). Net for a consumer: **637 MB → 161 MB, 3.96x**.

## Data license

The records are derived from English Wikipedia and Wikidata. Wikipedia text is
**CC BY-SA 4.0**; Wikidata is **CC0**. Anything redistributed from `out/`
carries those terms and needs the attribution.
