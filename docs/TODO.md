# filmstock — outstanding work

Status as of 2026-08-04, after the extract/index rewrite.

Pipeline today:

```
filmstock extract -dumps DUMPDIR -out OUTDIR    # dumps -> records (+ index.db by default)
filmstock index   -records OUTDIR               # rebuild index.db from records alone
filmstock serve   -db OUTDIR/index.db -movies OUTDIR/movies -television OUTDIR/television
```

Current data (2026-08-22, re-extracted after the tv -> television rename):
165,265 films · 4,669 events · 61,137 series · 551,202 episodes · 219,050 people
(148,030 with a Q-id, 67.6%) · 1,294,290 credits. Those are `index.db` rows;
219,628 people *records* were written, the difference being people with no
indexed credit.

Extract 7,459 s CPU / 1.9 GB peak RSS / 65.5 min wall, but the box was serving
other reads throughout, so wall time is not comparable to the 48.4 min recorded
on an idle box for 7,103 s CPU. CPU is the stable figure and moved <5%. The 17.4
min once recorded is not reproducible from a cold page cache. This is I/O bound
on the array, as the storage notes predict. Reindexing from records is 2m20s.

Films dropped from 170,421 because 5,156 records were never films: `findTemplate`
prefix-matched `{{Infobox film awards}}` and `{{Infobox Film festival}}` against
`{{Infobox film}}`. They are now `events` (3,039 award ceremonies, 1,630
festivals; 1,021 with a broadcaster, 2,375 host credits) — see event.go. The film
side now uses findTemplateExact, with a regression test.

## Retrieval

Search on main is lexical: FTS5 trigram over titles, cast and creators, which is
what a browser needs to find a specific entry. Everything beyond that — dense
embeddings, ColBERT late interaction, quantisation, reranking, fusion, and the
eval harness that scored them — lives on the **ai-experiments** branch, together
with the measurements that settled which of them were worth keeping.

## A. Record storage (gitdb)

Records are stored in gitdb stores, one per kind, so adding or changing a record
is a one-line diff in git. See github.com/szatmary/gitdb. Done and measured:

  450,701 records   376 MB working tree   361.53 MiB packed   117 files
  extract+index 43.1 min, was 83.7 — the parse runs at 11,741 pages/s against
  7,891, and indexing dropped 27.8 min -> 3.0, because reading 117 append-only
  files is a different workload from 450,699 tiny ones on a ~100 IOPS array.

- **UNTRUSTED: the people dictionary.** Trained on records averaging 36 bytes,
  because biographies are joined at the END of an extract pass and the bounded
  run used for training never reached most of them. Real people records carry a
  biography 54% of the time and are an order of magnitude larger. Its measured
  15.8% gain over a shared dictionary is real but says nothing about the corpus
  it will actually compress. Retrain with `filmstock train-dict` against a full
  store and rebuild. The events dictionary has the same problem for a different
  reason: 69 training records, which zstd itself warned was far too few.
- **Dictionaries are rebuilt often, not once.** A dictionary is only as good as
  the records it saw and the corpus changes with every dump. Changing one
  invalidates the store built with it — the identity is in the store header and
  a mismatch is refused at Open — so training is always followed by a rebuild.
- **Per-kind, not shared**: worth 15.8% on people and 27.4% on events over one
  shared dictionary, ~1.5% on films and series. Costs nothing structurally since
  each kind is already its own store.
- **page_id cannot be the store id.** 231,071 works over an 83.6M id space is
  0.28% density, and gitdb addresses a slot per id, so it would be 877 MB of
  tombstones. The index maps page_id -> store id; nothing derives either from
  the other. The cost is that a record's location is no longer a pure function
  of its identity: a reader now needs the index, and re-extract must read the
  store before writing it.
- **Open: television in incremental ingest.** `filmstock update` applies a day's
  adds-changes dump, but skips television. A season article names its season and
  never its series, and that series may not be in the day's changes, so attaching
  it means merging into a record the pass never sees.
- **Open: page deletions.** An adds-changes dump carries pages that CHANGED; a
  page deleted from Wikipedia stops appearing, which is indistinguishable from
  one that did not change. Only a full pass or a separate page list finds those.
  A page that stops QUALIFYING is already handled, because that page does appear.
- **Settled: gitdb_id stays in the index, for readers only.** Not for deletions
  and not for updates — both derive the mapping by scanning, and both are
  offline. It is there because anything opening a record by page_id would
  otherwise pay ~23s and ~40 MB to rebuild the map in memory. 8 bytes a row in a
  file that is not committed.
- **Considered and declined: storing file+offset instead of gitdb_id.** It saves
  one seek, and one HTTP round trip if remote reads ever return. Declined because
  a gitdb id is permanent across updates and compaction while a location is
  exactly the volatile half; because a stale location can land on a different
  record's start and return the wrong film silently, where a stale id errors; and
  because reading a location directly means reimplementing gitdb's format outside
  gitdb — which went v4 -> v5 mid-session, changing that very packing. Revisit if
  remote reads return, and then as a validated hint rather than the source of
  truth.

## B. Identity and data quality

- **Duo articles give both people the same Q-id.** "[[Jonathan Dayton and Valerie
  Faris|Jonathan Dayton<br>Valerie Faris]]" yields two people sharing one link
  target, so keying on it merges them — the John Williams bug in miniature.
  Needs per-name resolution against Wikidata labels rather than the shared article.
- **1,705 episode sources unattached** — mostly anime seasons and episode lists
  (`Bleach season 6`, `KonoSuba season 3`) where nothing states the relation.
  Counted, never guessed. Only fixable if a stated edge exists.
- **~40 "people" are concept words** linked to concept articles (`dialogue` →
  [[Dialogue]], `assist.` → [[Film editing]]). 0.02% of people. Fixing needs a
  concept blocklist, which risks suppressing real people — judged not worth it.
- **Comma-separated person fields are not split** (95 rows), but most hits are
  legitimate (`Allen G. Siegler, A.S.C.`, `Earth, Wind & Fire`). Low value.

## C. Wikidata — one more pass, enumerate BEFORE running it

The 102 GB dump is read in one pass; anything not extracted costs a full re-read.
Scope everything up front rather than discovering gaps mid-run (that mistake was
made twice already this session).

- **External IDs**: P345 (IMDb), P4947 (TMDB movie), P4983 (TMDB TV), maybe P1258
  / P1712. These are the join key every media manager (Plex/Jellyfin/Sonarr/Radarr)
  uses, turning "match a local file to a record" from fuzzy title matching into an
  exact join. The one thing Wikidata offers that enwiki infoboxes cannot.
- **P1545** series ordinal, and whatever the music module will need (performer,
  tracklist, release) — decide before the pass, not during.
- **`wd_text` has no consumer yet.** 10.3M rows of labels/descriptions/aliases in
  ~300 languages were collected and nothing reads them. Baking them into the
  records would make records self-contained and give multilingual search; it also
  answers the display-ambiguity problem (50 series all rendering as "Big Brother").

## D. Distribution (git-annex + signed commits + p2p)

**Settled:** the record tree is committed, in its own repository —
`filmstock-data`, 450,699 records, 437.9 MiB packed. Separate from the code
because the Go module proxy zips the whole module tree and caps it at 500 MiB.
The index is not committed: a rebuild changes 100% of its bytes (measured), so it
would cost ~383 MB of history per ingest to store something that regenerates in
2m20s. `filmstock split`/`join` exist if that trade is ever worth making.

- **Records are byte-deterministic** — verified: identical input gives identical
  bytes (Go's gzip writes no timestamp, encoding/json sorts map keys). This is the
  precondition; without it every re-extract churns all ~620k files.
- **RESOLVED, and quantified.** A re-extract of the same dump into the existing
  store leaves 450,295 of 450,634 records byte-identical — 99.925%. Films and
  events are exactly deterministic, 0 of 165,265 and 0 of 4,669. The 339 that
  differ are 307 people and 32 television, entirely in the merge paths. A no-op
  re-ingest costs ~1,000 changed lines of 901,456, so the repository grows by
  megabytes per ingest rather than hundreds of them.

  The earlier appearance of wholesale churn was our own bug, not nondeterminism:
  storeWriter called gitdb's Update unconditionally, and Update appends a new
  version whether or not the content changed. Fixed by comparing first.

  **Still open**: the residual 339. Both merge paths assemble from data arriving
  in goroutine-scheduling order — television from a collector fed by a channel,
  people from a map flushed at the end — so the surviving nondeterminism is
  almost certainly ordering, not parsing. Worth fixing if byte-exact reingest
  matters; ~0.075% of records is not worth much else.

- ~~**SUPERSEDED — the 2026-08-22 re-extract did not reproduce two counts.**~~ Same
  dump, same resolver cache: films, events, series and people all landed on the
  exact same numbers, but `television_episodes` moved 551,174 -> 551,202 (+28)
  and `credits` moved 1,294,389 -> 1,294,290 (-99). Both are merge paths rather
  than keyed lookups — episodes gathered across source articles, credits deduped
  through the `seen` map in televisionindex.go — which is the shape map-iteration
  order produces. It is not proven: the earlier figures may predate a code change,
  and this is one run against one recorded set. **Settle it before building
  anything on diffs**, because determinism is this whole section's precondition:
  extract twice into scratch directories on identical code and `diff -r` the
  record trees (~65 min each). If they differ, the diff/sync plan needs an
  ordering fix first.
- ~~**Reconsider gzip**~~ — MEASURED, and it does not matter. 8,000 real records,
  second ingest changing 1% of them: plain `.json` 12.5 MB then +0.1 MB; gzipped
  12.8 MB then +0.1 MB. Extrapolated to all films, plain costs 259 MB and gzipped
  264 MB, and a re-ingest is 1 MB against 3 MB. The gzip penalty is real per-file
  and irrelevant at this size, because a changed record is only ~1.5 KB. Plain
  JSON is 3.1x larger on disk for no packfile benefit. Keep gzip.
- **Plain git probably beats git-annex** for `movies/`/`television/`/`people/` (5-20 KB,
  highly deltable). Annex suits `text/` (1.3 GB), if it ships at all.
- ~~**620k files** makes `git status` slow~~ — MEASURED, not a problem. The real
  tree is 450,699 records: `git add` 22.8 s, `commit` 1.1 s, `gc` 30 s,
  `git status` **0.28 s**, packfile 437.9 MiB. No `feature.manyFiles`, untracked
  cache or fsmonitor needed at this scale.
- **Stamp records with an extractor version** so consumers can distinguish "the
  data changed" from "the parser changed". A parser change rewrites every record.
- Manifest of `page_id -> content_hash` for diffs — mostly free once records are
  deterministic and git-tracked.

### D1. Ship the database; fetch records on demand

The consumer should not have to take 60 GB to look up a film. `index.db` (637 MB)
already carries everything the search and list views need — titles, years,
credits, people, episodes, and the FTS indexes. The per-record `.json.gz` only
adds the detail page: raw infobox, nested seasons and episodes, plot, overview.
So the natural split is **the database is the download, the records are fetched
per view**.

`serve` is already shaped for this. `recordPath` makes a record's location a pure
function of `(kind, id)`, and only two call sites read one (`serve.go` for the
film and series pages). One `recordSource` interface with a local-directory and a
remote implementation covers it.

**Do not serve 620k loose files.** That is the obvious version and it is the bad
one: a TLS handshake and a round trip per record, no way to list or verify what
exists, raw-file hosting rate limits, and the `git status` problem above simply
moved onto the network. Instead:

- **One `records.pack`** — every record concatenated, each still individually
  gzipped so it stays independently decodable.
- **Offsets live in `index.db`** — add `(pack_offset, pack_length)` beside each
  work. About 8 bytes a row in a file the client already has, so locating a record
  costs zero extra requests.
- **Fetch with an HTTP Range request** — one round trip, ~5 KB, CDN-cacheable, and
  supported by every host worth using (GitHub Release assets, S3, R2).

Sizes today, measured by `filmstock pack` rather than estimated: movies.pack
240 MB, television.pack 150 MB, events.pack 3.5 MB — **394 MB total**. An earlier
note here claimed 1.9 GB; that was `du` output, which counts filesystem block
overhead across 620k small files rather than data, and it overstated the real
figure roughly fivefold. Every asset therefore sits far under GitHub's 2 GB
per-asset limit, with room to grow.

People are deliberately not packed: a person record is `{name, qid, wiki}` and
all three are already columns in the database, so fetching one would return
nothing new. `out/text/` (the full-text corpus) is not packed either — it has no
consumer on main.

This composes with the diff work rather than competing with it: records are
byte-deterministic, so a `page_id -> content_hash` manifest tells a client exactly
which ranges changed between two ingests, and it re-fetches only those.

Settled by building it: the pack is rebuilt whole per ingest. Re-uploading
394 MB is trivial next to a 65-minute extract, and appending with a tombstone
list would complicate deletion, which ingest does not handle at all yet.

Built and tested: `filmstock pack` writes the packs and the offsets;
`filmstock.Remote(baseURL)` reads them by range. Dir and Remote were verified to
return byte-identical records, 2.9 ms against 4.8 ms over HTTP.

## E. Housekeeping

- **The people store is walked twice per index run.** loadPeopleQIDs builds the
  link-target -> identity map by walking every person record, and both CIndex and
  CIndexTelevision call it — ~220k records inflated and JSON-decoded twice for
  the same map. This was nearly free when records were loose files on a warm page
  cache; with gitdb every record goes through zlib with a preset dictionary, so
  the second walk is real CPU. Build it once in CIndexRecords and pass it down.
  Found by asking what the index phase actually reads, not by profiling.

- **A progress stream that dies when piped is not a progress stream.** The
  ticker writes \r-terminated lines to stderr; the wrapper scripts pipe stderr
  through grep, which block-buffers, so an 85-minute run produced a 181-byte log
  and no visible progress at all. Whatever replaces it has to survive a pipe.

- **extract has no progress percentage or ETA.** The ticker prints elapsed,
  pages scanned, rate and films found, with no denominator — you have to know
  from memory that the dump is ~25.7M pages. Use BYTES, not pages: the total
  page count is only known when the pass ends and changes with every dump,
  whereas RunMultistream already stats the dump for its size and loads the
  offset index, and dispatches work as byte ranges. Byte progress also predicts
  time honestly because the job is I/O bound — the current pages/s reads 174/s
  early and 7,891/s later, which is article size varying, not the job speeding
  up 45x. Wants a progress func(done, total int64) callback on RunMultistream.

- ~~`serve` defaults to the old layout~~ — DONE. Every flag default is now
  relative to the working directory and points at the records layout (`dump`,
  `out`, `out/index.db`, `out/movies`, `out/television`), so the tools run from
  the repo root with no arguments.
- Old artifacts still on disk: `movies/`, `television/`, `text/`, `movies.db` (pre-rewrite),
  and `wikidata.db` (28 GB resolver cache, build-time only, discardable).
- **`/tank` is 5 USB 3.0 disks in raidz1** — ~100 IOPS. The record-per-file model
  is near-worst-case for it on both write and read. Moving `-out` to the NVMe at
  `/` (1.3 TB free) would cut both extract and index time substantially. Dumps and
  corpus are bulk sequential and belong on /tank.
