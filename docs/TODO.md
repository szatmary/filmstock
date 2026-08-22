# filmstock — outstanding work

Status as of 2026-08-04, after the extract/index rewrite.

Pipeline today:

```
filmstock extract -dumps DUMPDIR -out OUTDIR    # dumps -> records (+ search.db by default)
filmstock index   -records OUTDIR               # rebuild search.db from records alone
filmstock serve   -db OUTDIR/search.db -movies OUTDIR/movies -television OUTDIR/television
```

Current data (2026-08-13): 165,265 films · 4,669 events · 61,137 series ·
551,174 episodes · 219,629 people (148,220 with a Q-id, 67.5%).
Extract 48.4 min wall / 7,103 s CPU / 1.7 GB peak RSS, measured on an otherwise
idle box. The 17.4 min previously recorded is not reproducible from a cold page
cache — CPU is only 2.4 cores' worth across 48 minutes, so this is I/O bound on
the array, as the storage notes predict.

Films dropped from 170,421 because 5,156 records were never films: `findTemplate`
prefix-matched `{{Infobox film awards}}` and `{{Infobox Film festival}}` against
`{{Infobox film}}`. They are now `events` (3,039 award ceremonies, 1,630
festivals; 1,021 with a broadcaster, 2,375 host credits) — see event.go. The film
side now uses findTemplateExact, with a regression test.

## Retrieval, measured (415-query eval set, depth 20)

| path | MRR | concept | conjunctive | person | title | typo |
|---|---|---|---|---|---|---|
| lexical | 0.198 | 0.000 | 0.002 | 0.002 | 1.000 | 0.884 |
| dense (bge-large, int2->int8) | 0.382 | 0.217 | 0.511 | 0.206 | 0.938 | 0.630 |
| **ColBERT rerank** | **0.575** | 0.426 | 0.858 | 0.474 | 0.977 | 0.553 |
| RRF fused (lexical+ColBERT) | 0.508 | 0.297 | 0.781 | 0.360 | 0.983 | 0.783 |

Settled by measurement, do not re-litigate:
- **Fusion loses.** Equal-weight RRF scored below the better input in three
  independent experiments. Route by query shape instead: lexical owns title
  (1.000) and typo (0.884), ColBERT owns everything else.
- **The ColBERT store must stay int8.** int4 -> MRR 0.456, int2 -> 0.073, even
  though the vectors survive to 0.996 cosine. MaxSim scores span ~1% between
  passages, so a 1-2% per-passage error exceeds the whole signal. Quantising the
  residual from the mean token vector (-center) is worth about a bit — int4c
  0.591, int2c 0.289 — and still loses. Centroid-residual (PLAID) is the only
  compression with a chance.
- **A bigger encoder did not help.** GTE-ModernColBERT (ModernBERT, dim 128,
  19.83 GB, 1 h GPU) scored 0.582 against answerai-colbert-small's 0.697 on the
  old 38-query set. Verified not a plumbing bug: store reproduces float32 within
  1%, skiplist correct, both models near-perfect against random distractors.
- **A cross-encoder third stage did not help.** bge-reranker-base scored 0.523
  (passage-level) and 0.430 (after roll-up) against 0.668 for not reranking.
- **Depth is the only lever that has ever helped**: 0.627 @50, 0.668 @100,
  0.697 @500, 0.714 @1000 — still climbing, so candidate recall from the dense
  stage is the binding constraint. That is the argument for PLAID retrieval.

Latency: the 13.25 GB store is seek-bound on /tank — 31 s/query cold, 0.4 s/query
once page-cache warm, a 36x difference with identical scores. It belongs on the
NVMe, or warmed at startup.

---

## A. Semantic search — the current priority

**Next**: PLAID centroid retrieval. It is the only remaining lever the evidence
supports — it removes the candidate-recall cap AND is the compression scheme that
the int8 finding says we need. Everything else measured neutral or negative.

Items 1-7 below are DONE (eval harness, chunker, GB10 embedding job, quantizer,
in-process cascade, film-space browser, and both retrieval paths). What is left
is listed under "open" at the end.

1. ✅ **Eval harness FIRST.** 38 queries in `docs/eval/queries.json`, scored by
   category — `filmstock eval` (lexical), `eval-vec` (dense), `eval-colbert` (late
   interaction). Without it the silent failure modes (pooling mismatch, missing
   `query:`/`passage:` prefixes, wrong model version) are indistinguishable from
   "the model is mediocre".
2. ✅ **Chunker** (`chunk.go`) — section-aware, contextual headers, and three
   corpus profiles (small/medium/large) for browser / Pi / server.
3. ✅ **Offline embedding job** (`embed/embed.py`), bge-large-en-v1.5 at 1024 dims,
   606,570 passages.
4. ✅ **Quantizer** (`quantize.go`) — per-dimension calibration → int8 (0.9999
   cosine) + int2 (0.9430).
5. ✅ **Query embedder** — currently the Python sidecar (`embed/testui.py`). The
   pure-Go static/Model2Vec device encoder is still open.
6. ✅ **In-process cascade** (`vecsearch.go`) — int2 scan → int8 rerank → roll-up.
7. ✅ **RRF fusion** (`fuse.go`) — implemented and wired, but it MEASURES WORSE
   than either path alone (0.360 vs 0.516 vec-only) and is not the default.
   Naive equal-weight RRF drags concept and person queries down; route by query
   shape or weight the lists before turning this on.
8. ✅ **Second path: ColBERT late interaction** — `embed/colbert.py`,
   `src/colbert.go`, `colbert.sh`. 138M token vectors, 13.25 GB, server-only.
   See `docs/embeddings.md` §11.

**Open**
- **Television corpus**: the passage corpus is films only (170,421 works). 61k
  series and 551k episodes have no embedding text at all — one enwiki pass
  (~17 min).
- **step2 must be separated.** `step2.sh` changed three things at once
  (section-aware chunking, contextual headers, lean profile) and REGRESSED: MRR
  0.516 against the 0.602 baseline. Its own header says they have to be split if
  it did not improve. Also: `out/passages2.jsonl.gz` was written at 09:14 and
  `chunk.go`/`filmstock` were rebuilt at 09:16, so that score may not even reflect
  the chunker that produced it — re-chunk before drawing any conclusion.
- **Pure-Go query encoder** (static/Model2Vec), so the device path needs no
  Python at all.

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

- **Records are byte-deterministic** — verified: identical input gives identical
  bytes (Go's gzip writes no timestamp, encoding/json sorts map keys). This is the
  precondition; without it every re-extract churns all ~620k files.
- **Reconsider gzip.** Git already zlib-compresses, and delta compression cannot
  see through gzip — a one-field change re-transfers the whole blob. Plain `.json`
  costs ~3-4x locally but makes packfiles and p2p deltas far smaller.
- **Plain git probably beats git-annex** for `movies/`/`television/`/`people/` (5-20 KB,
  highly deltable). Annex suits `text/` (1.3 GB), if it ships at all.
- **620k files** makes `git status` slow: set `feature.manyFiles`, untracked cache,
  fsmonitor; sparse-checkout lets consumers take only what they want.
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

Sizes today: movies 1.2 GB + television 495 MB + people 129 MB + events 36 MB ≈
1.9 GB, which fits under the 2 GB per-asset limit on a GitHub Release — but only
just, so it needs a size check per ingest and a plan for splitting by kind when it
grows. `text/` (1.3 GB) is a second pack that only the semantic path needs.

This composes with the diff work rather than competing with it: records are
byte-deterministic, so a `page_id -> content_hash` manifest tells a client exactly
which ranges changed between two ingests, and it re-fetches only those.

Open question before building it: whether the pack is rebuilt whole per ingest
(simple, and re-uploading 1.9 GB is cheap next to a 48-minute extract) or appended
to with a tombstone list (complicates deletion, which ingest does not handle yet
anyway).

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
