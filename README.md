# filmstock

A media database built from the English Wikipedia and Wikidata dumps: films, television
series and episodes, award ceremonies and festivals, and the people credited on
them — parsed, cross-linked by Wikidata Q-id, and indexed for search.

No API keys, no scraping, no third-party service. The dumps go in; three SQLite
databases come out, ready to serve:

| file | holds | size |
|---|---|---|
| `filmstock.db` | every entity, credit and search index | 315 MB |
| `filmstock-text.db` | overviews, plots and episode summaries | 614 MB |
| `filmstock-vectors.db` | embedding vectors for recommendations | 222 MB |

**Current build** (enwiki 2026-08-01 dump):

| | rows in `filmstock.db` |
|---|---|
| films | 165,740 |
| award ceremonies & festivals | 4,685 |
| television series | 65,025 |
| television episodes | 667,800 |
| people | 235,890 |
| credits | 1,414,524 |

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
  unattached and **counted** rather than guessed at.

## The pipeline is two passes

```
import   dump -> intermediate.db     every recognised page, wikitext kept (~7.6 GB)
export   intermediate.db -> the dbs  every record re-derived from the whole corpus
```

The intermediate is what makes the daily update honest: a day's adds-changes
dump goes into the intermediate (`filmstock update`, ~140 s), and export then
re-derives **everything** from the whole corpus. A new film's cast were not
edited on the day the film was — only a full re-derivation can credit them.
Exports are byte-deterministic: the same intermediate produces the same
database, which is what lets releases be verified and diffed.

## Build it

You need Go 1.26+, a C compiler (see *The SQLite inside*), `sqlite3`, and
`lbzip2` (or `bzip2`). Budget ~130 GB for the dumps and ~28 GB for the
build-time resolver cache.

```sh
make build          # -> ./filmstock
make dumps          # ~129 GB into ./dump — go do something else
make resolver       # wikidata.db: P179/P4908 edges, then the Q-id map
filmstock import -dumps dump -inter intermediate.db     # pass 1
filmstock export -inter intermediate.db -db filmstock.db # pass 2
```

`filmstock extract` runs the whole chain (resolver phases included) from the
dumps in one command. Daily updates after a full build:

```sh
filmstock catchup -db filmstock.db      # apply every daily the intermediate is behind
```

### Serve it

```sh
filmstock-web -db filmstock.db          # http://localhost:8080
```

`filmstock-web` finds `filmstock-text.db` beside the main database; pass
`-vectors` to enable the `/explore` recommendation walk.

## The SQLite inside

The library compiles its own SQLite. `internal/sqlite3` carries the SQLite
3.53.4 amalgamation and the go-sqlite3 binding (vendored, see its README for
provenance), built by cgo with **FTS5 enabled unconditionally** — importers
get a full-text-capable engine with no build tags and no system libsqlite3.
The driver registers as `filmstock-sqlite3`, so it can never collide with an
application's own sqlite3 driver.

Building with `-tags filmstock_purego` (or `CGO_ENABLED=0`) swaps in the
pure-Go `modernc.org/sqlite` driver instead. Both drivers produce identical
content hashes over the published databases — verified on every release.

## Layout

```
github.com/szatmary/filmstock        the library — import this
  db.go              Open, DB, the search methods, ErrNotFound
  titleinfo.go       FilmInfo / SeriesInfo batch lookups
  search*.go         lexical search: FTS5 trigram + fuzzy ranking
  updater.go         consumer auto-update: fetch, verify, swap
  contenthash.go     canonical content hash over every published table
  movie.go television.go event.go people.go     record types
internal/
  sqlite3/           vendored SQLite amalgamation + binding (cgo)
  sqldrv/            driver selection (cgo default, purego fallback)
  wikitext/          template and infobox parsing, link/ref cleanup
  dump/              multistream bz2 reading off the offset index
  build/             import/export pipeline, resolvers, release tooling
cmd/
  filmstock/         the CLI — dispatch only
  filmstock-web/     the browser
```

## Using the data

**The product is three SQLite files and [a documented schema](docs/schema.md).**
Open them with any SQLite library, in any language. There is no query API to
learn and none to be limited by: your questions are yours, and SQL answers
more of them than a set of methods would.

```sql
ATTACH DATABASE 'filmstock-text.db' AS text;

SELECT m.title, m.year, t.plot
FROM movies m JOIN text.movie_text t ON t.id = m.id
WHERE m.year = 1954;
```

Two things to know before writing that query, both covered in
[docs/schema.md](docs/schema.md): every id is the article's Wikipedia page_id,
and the full-text indexes ship empty because they are derived data that
rebuilds in seconds.

### The Go package

For Go consumers it does the parts you would not want to reimplement —
opening the files together, and keeping them current:

```go
db, err := filmstock.Open("filmstock.db",
    filmstock.Attach{Schema: "text", Path: "filmstock-text.db"})
defer db.Close()

rows, err := db.SQL().Query(`SELECT title FROM movies WHERE year = ?`, 1954)
```

`Updater` fetches a release, verifies file and content hashes, applies daily
patches when the catalog offers a chain, rebuilds the FTS tables, and swaps
the live handle atomically so a running server never closes its database to
take an update.

## Releases

`manifest.json` states each file's size, sha256 and content hash;
`builds.json` is the catalog — every full and daily build, each daily naming
its parent, plus a bridge diff from the previous chain's tip to the new full
so a consumer can verify the chains agree. Content hashes are computed over
canonical row orderings, so they survive VACUUM, page-level differences and
driver changes.

## Data license

The databases are derived from English Wikipedia and Wikidata. Wikipedia text
is **CC BY-SA 4.0**; Wikidata is **CC0**. Redistribution carries those terms
and needs the attribution.
