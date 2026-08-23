# filmstock — outstanding work

Status as of 2026-08-04, after the extract/index rewrite.

Pipeline today:

```
filmstock extract -dumps DUMPDIR -out OUTDIR    # dumps -> records (+ search.db by default)
filmstock index   -records OUTDIR               # rebuild search.db from records alone
filmstock serve   -db OUTDIR/search.db -movies OUTDIR/movies -television OUTDIR/television
```

Current data (2026-08-22, re-extracted after the tv -> television rename):
165,265 films · 4,669 events · 61,137 series · 551,202 episodes · 219,050 people
(148,030 with a Q-id, 67.6%) · 1,294,290 credits. Those are `search.db` rows;
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
- **UNRESOLVED — the 2026-08-22 re-extract did not reproduce two counts.** Same
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

The consumer should not have to take 60 GB to look up a film. `search.db` (637 MB)
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
- **Offsets live in `search.db`** — add `(pack_offset, pack_length)` beside each
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

- ~~`serve` defaults to the old layout~~ — DONE. Every flag default is now
  relative to the working directory and points at the records layout (`dump`,
  `out`, `out/search.db`, `out/movies`, `out/television`), so the tools run from
  the repo root with no arguments.
- Old artifacts still on disk: `movies/`, `television/`, `text/`, `movies.db` (pre-rewrite),
  and `wikidata.db` (28 GB resolver cache, build-time only, discardable).
- **`/tank` is 5 USB 3.0 disks in raidz1** — ~100 IOPS. The record-per-file model
  is near-worst-case for it on both write and read. Moving `-out` to the NVMe at
  `/` (1.3 TB free) would cut both extract and index time substantially. Dumps and
  corpus are bulk sequential and belong on /tank.
