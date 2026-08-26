# filmstock — outstanding work

Status as of 2026-08-26.

Current data (2026-08-01 dump, re-extracted 2026-08-26):
165,740 films · 61,342 series · 572,847 episodes · 234,778 people ·
4,705 events · 232 schedule grids (48,535 slots). Extract 41 min with the
resolver cache warm; 70 min cold, the difference being the 96 GB Wikidata pass.

---

## Open

### Daily updates cannot add people — BUILT, NOT YET PROVEN

`applyBiography` only ever updated a person already in the store; the daily path
had no equivalent of `notePeople`/`flushPeople`. A film added today brought a
cast list, and any genuinely new person in it got an index row from the credit
but no store record, no page_id, and no biography.

Measured: 478 such people arrived across 23 daily updates, none with a canonical
identity. ~21/day, monotonic, never reconciled without a full re-extract. It
matters more than the rate suggests because new releases are what gets looked up.

Built as designed in [TWO-PASS.md](TWO-PASS.md):

    filmstock import -dumps DUMPS -inter intermediate.db     pass 1, full dump
    filmstock import -incr DAY.xml.bz2 -inter intermediate.db  pass 1, one day
    filmstock export -inter intermediate.db -out RECORDS      pass 2

Import keeps every person rather than only the credited ones, so a daily update
resolves a new film's cast against the whole corpus instead of one day of it.
Export is `extract`'s second half reading the intermediate through the same
`pageSource` the dump satisfies — one implementation, two readers, because a
second copy of the shaping logic would drift silently.

**Verified.** The first full import ran in 38.7 min (extract: 41) producing a
7.6 GB store — 1,206,604 claims over 1,206,548 pages, at 11,112 pages/s by the
end. Export from it takes **5.8 min**, and reproduces extract exactly where it
should:

| kind | result |
|---|---|
| movies | 165,740 identical, 0 changed |
| events | 4,685 identical, 0 changed |
| television | 52,192 identical, 9,150 changed — all in `seasons` |
| people | 234,285 identical, 490 changed — all in `name` |

The events figure also settles an old discrepancy: this TODO said 4,705, and
the current records hold 4,685. The number was stale, not a loss.

**What remains before this can be believed:**

1. The first full import is running. Rate climbed past 4,700 pages/s once into
   the body of the dump; ~11.7 kB per kept page gzipped.
2. Export has never been run against real data. The claim that a record built
   from the intermediate is the record `extract` would have built is tested only
   on synthetic pages so far. **Diff a full export against the current records
   before trusting it.**
3. `-incr` has never been run against a real day.

### Records vary between identical runs — FIXED

Exporting the same intermediate twice, with the same binary, gave different
records: 174 television and 454 people. Movies and events were stable. Five
causes, all the same shape — a Go map iterated in random order with two keys
writing one field — found by diffing export pairs:

| after fixing | television | people |
|---|---|---|
| (baseline) | 174 | 454 |
| episode-list ownership | 164 | 453 |
| person name, source merge order | 36 | 1 |
| part figures (`infoA14S`) | 9 | 0 |
| column roles (`Network Rank`) | **0** | 1 (see below) |

The one remaining people record is not an ordering bug: it is the redlink hash
collision, which has its own entry.

None of these were visible before. Finding them needs the pipeline re-run
against a fixed input and diffed against itself, which took 41 minutes and a
dump before the intermediate existed and now takes six minutes.

**Episode-list ownership was decided by map iteration order.** More than one
series can name the same episode-list article — 51 lists claimed by 530 series
between them — and `buildListOwner` wrote each claim straight into a map, so
the owner was whichever series Go reached last. I Love Lucy's six seasons and
180 episodes attached to it on one run and to The Lucy–Desi Comedy Hour on the
next. Now: an unfragmented link claims the article, a `#section` link claims a
section, ties break on lowest page_id.

**A person's name came from whichever credit arrived first.** 454 people per
run flipped between spellings of their own name — "Bob Colleary"/"R.J.
Colleary", "Yvette González-Nacer"/"Yvette Gonzalez-Nacer". Identity never
moved; only the label. The name now comes from the article title.

**A part's Nielsen figure overwrote the season's.** Vera writes `infoA14 = 6.24`
for season 14 and `infoA14S = 3.11` for its specials; both matched the parameter
pattern, so the same text parsed twice in the same process gave different
answers. Introduced by accepting a trailing letter so split seasons keep their
air dates.

**Two columns claimed the same role.** Charmed's overview has "Rank" and
"Network Rank"; Once Upon a Time has "Viewers rank" and "18-49 rank". Both
wrote `Season.Rank`. The first column to claim a role now keeps it — the
primary figure comes first in every case in the corpus.

This matters more than the counts suggest: the store is distributed as a git
history, so every run rewrote hundreds of records that had not changed.

### Bands are being parsed as people — FIXED

6,981 person articles are disambiguated "(band)". `{{Infobox musical artist}}`
serves both individuals and groups and says which — `background =
group_or_band` — and `buildBiography` never asked, so X Japan and Wolfmother
carried biographies with unfilled birth-date parameters. 589 records lost a
biography they should never have had.

Only where the article states it: about half the "(band)" articles omit the
parameter, and a title is a display string, not evidence. Those are untouched.

They remain PersonRecords — a band credited for a film's music is a real
credit — they simply no longer claim to be a person. Whether a credited group
should be its own kind is a separate question.

### Multiseries {{Series overview}} yields nothing

Shows that share an episode list nest one overview per series inside an outer
one, each numbering its seasons from one. Reading them into a list keyed by
season number alone would give one show's Nielsen rank to another's season, so
they are skipped. Doing it properly means routing each nested block to the
series its `series =` parameter names. I Love Lucy and The Lucy–Desi Comedy
Hour are the worked example.

### The reference graph misses list_episodes entirely

`linksOf` reads links out of infobox fields with `SplitLinks`, which requires a
`[[wikilink]]`. `list_episodes` is written as a bare page title, so the links
table holds **0 of 61,342** such edges. Harmless today — the export path
resolves the field directly — but the graph is what incremental export will use
to decide what went stale, and a missing edge there is a silently stale record.

### Compressing the published database — DEFERRED, PLAIN SQLITE FOR NOW

Decided: ship a plain SQLite file. Revisit compression once the pipeline is
producing it daily and we know whether at-rest size is a real constraint.

Everything below is measured, so the decision can be reopened without redoing
the work.

**Sizes, as built.** Without synopses the database was 409 MB (174 content, 152
FTS5, 82 indexes) and compressed to 204 MB. With them it is **974 MB on disk and
308 MB compressed** — prose compresses 3.2x, far better than the rest. Adding
the embedding vectors (170,421 x 1024 int8, effectively incompressible) would
make it ~1.15 GB on disk and ~480 MB to download.

The text is 462 MB, and 157 MB of it is episode summaries — 583,401 of them,
which is easy to overlook next to the films:

| column | size |
|---|---|
| movies.plot | 174 MB |
| episodes.summary | 157 MB |
| movies.overview | 66 MB |
| tv.plot | 34 MB |
| tv.overview | 31 MB |

**A split may serve better than compression.** Core / text / vectors as three
files, ATTACHed into one logical database: a consumer wanting a media database
takes 204 MB, one wanting synopses adds them, one doing ML search adds the
vectors. It also keeps daily patches per-file. Worth weighing against
compression rather than after it.

| approach | result |
|---|---|
| whole-file `zstd -3` | 409 -> **204 MB**, 1.4 s |
| whole-file `zstd -19` | 409 -> **169 MB** |
| per-row gzip on synopses | 253 -> 123 MB (2.06x) |
| per-row flate + 110 KB dictionary | 253 -> 104 MB (2.43x) |
| zstd on int8 vectors | 38 -> 32 MB (1.19x) — effectively incompressible |

**Whole-file compression beats every scheme we can build**, because its window
is the entire corpus while a page compressor sees 4 KB and a deflate dictionary
sees 32 KB — deflate's window caps the dictionary, which is why the hand-rolled
one only reached 2.43x. Page compression's advantage is random access without
decompressing everything, not ratio.

**Page-level options, if at-rest size ever matters.**

- `sqlite_zstd_vfs` (mlin) — open source, **read/write**, ACID-preserving by
  design, and it trains dictionaries automatically. Stores inner pages as rows
  of an outer table, so the compressed file is still a SQLite database rather
  than an opaque blob. Conversion is one statement:
  `VACUUM INTO 'file:out.db?vfs=zstd'`. Reports ~40% on their example, where
  whole-file zstd gives us 50%. Author's caveat is worth quoting: *"USE AT YOUR
  OWN RISK... young and unlikely to have zero bugs. This project is not
  associated with the SQLite developers."*
- ZIPVFS — the SQLite authors' own, and commercial. Needs cgo and a licence.

Both need cgo, which we can have; the cost is that the published file stops
being readable by stock SQLite. The likely shape is publish plain and let
Grindhouse convert locally with `VACUUM INTO`, so the dataset stays universal
and the app still gets a small file.

**Compression and diffing fight each other.** Change one row, its page
recompresses, every byte of that page changes. That kills page-level and
rsync-style deltas. It does NOT affect us, because a day's patch is emitted as
SQL by the exporter — which already knows exactly which records changed — not
by diffing two files. `sqldiff` is not involved and could not be: it has no
`--vfs` flag, and against a page-compressed file without the VFS it would diff
the compressed page table and emit nonsense.

That is what makes the three concerns independent: **transport** (zstd the
file), **at rest** (plain, or the consumer converts), and **daily patch** (SQL
from the exporter) can each be settled on their own.

**Vectors do not require `sqlite-vec`.** Stored as plain BLOBs with kNN in Go,
brute force over 170,421 x 1024 int8 is well under a second, which `vectors.go`
already does. The extension buys SQL-level `MATCH` and ANN indexing we do not
need at this scale, and costs the file being openable without it. Per-row blobs
rather than one matrix, so a daily patch touches the vectors that changed
instead of rewriting 175 MB.

### 56 schedule articles still yield nothing

232 of 288 read. The remainder are the overnight, morning and afternoon variants.
Every schedule bug so far has been format variance that fails *silently* —
uppercase "7:00 PM" (cost the 1950s entirely), a second `{{small|…}}` note
(cost every Nielsen figure in a cell with a tie), day RANGES and level-3
headings (cost 154 articles each, independently). Assume another shape.

### The schedules dictionary is a placeholder

Bootstrapped from five parsed articles at a 7× source:dictionary ratio, which
zstd warned about; it is now compressing 232 grids. Retrain from the full set.

### Seasons — DONE

Seasons are first class: `PageID`, `Rank`, `Rating`, `Viewers`, `Network`,
`Starring`, `Image`, from `{{Series overview}}` (never parsed before) and the
full `{{Infobox television season}}` (previously read for two dates only).

Per-season `Starring` is the one that mattered. `TelevisionSeries.Starring` is
one flat list for a show's whole run, so a fifteen-season series asserted that
everyone who ever appeared was in it throughout — Clooney was in ER for five
seasons of fifteen. Not missing coverage: a modelling error recorded as fact.

Still unverified against real data, like everything else above — the parser has
tests but the corpus has not been run through it.

### 77,457 people have no canonical identity — DECISION NEEDED

A credit whose link target has no article has no page_id and no Q-id, so it is
keyed by a hash of the link target: a display string two people can share, which
changes if the article is later created. 32% of people, ~10% of credits, ~1.9
credits each. Either they are credits on a film rather than person records, or
they stay and the exception is permanent. Only the project owner can decide.

**The collision is fixed; the policy question is still open.**

Diffing two exports found key `-2070761073` holding **Issa Abdessamie** in one
run and **Costache Ciubotaru** in the other — two unrelated people, one record,
the loser silently overwritten. `PersonRecordPathID` was FNV-1a masked to 31
bits: at 77,457 redlinks that expects ~1.4 collisions and grows with the SQUARE
of the count.

It is now 64-bit (masked to 63, because the caller negates it). The same
population expects 3e-10 collisions, and 1e-8 at ten times the size. That was a
defect, not a decision, so it is simply fixed.

Keying them on the link target STRING was the other candidate and was not taken:
the read path keys `Location.ID` as an int, so it would ripple through the
index and fetch path — a cross-cutting refactor to support records that may not
survive the question below.

**What is still yours to decide:** should a credit whose link target has no
article be a person record at all? 32% of people, ~10% of credits, ~1.9 credits
each. The key is sound now but still not canonical — it derives from a display
string, so it changes if the article is ever created, and two genuinely
different people credited under one name still share it. Either they are credits
on a film rather than person records, or the exception is permanent.

### `omitempty` on published records — NOT A DECISION, MEASURED

Nothing to decide: the records already carry no empty fields. Dropping every
empty and null field from 5,000 movie records changes their size by **0.0%**,
because 23 of 27 movie fields already have the tag and the four without —
page_id, title and the like — are always populated. Same shape in the other
kinds. This entry was stale.


Absent keys rather than empty values. Smaller records, meaningful across 165k in
git, but every consumer must treat missing as unknown. Grindhouse hit this
looking for `plot`, which exists for 68.5% of films where `overview` exists for
100%. First real consumer, so the trade is now judgeable.

### Deletions

A page that disappears from Wikipedia is never removed: adds-changes dumps say
what changed, never what went away. Only a full pass can reconcile it. The
two-pass restructure would make this fall out for free.

### Wikidata cache staleness

The resolver cache is rebuilt only with a full extract, so television
relationships stated since are unknown — 1,726 unresolved seasons and drifting.
wikidatawiki publishes daily incrementals; the cache could be brought forward a
day at a time instead of rebuilt in 46 minutes.

### Infrastructure

- **SSD move.** 227 GB on five USB disks in raidz1 (~20 IOPS each). Measured 15×
  on the workload that hurt: moving one file off it took a 13-hour projection to
  51 minutes. NVMe has 2.3 TB free. Leave the 47 GB of cold ColBERT stores.
  RAM/tmpfs measured only ~15% over NVMe here — not worth it, the workload is
  CPU-bound in `lbzcat` and SQLite already runs `synchronous=OFF`.
- **CI workflow.** Everything it needs exists: `catchup` is one command, the slim
  cache is 1.03 GB (fits the 10 GB Actions cache), peak runner disk ~2.3 GB of
  ~14. Continuous publishing was chosen, so it can push.
- **Data push.** 23 commits and a full rebuild uncommitted. Wants compact +
  squash first: ~434 MB in one commit rather than 605 MB of history.

### Smaller

- 11 film titles still carry an odd parenthetical the disambiguator regex leaves.
- `filmstock search` covers only films; the library covers all five kinds.
- Episode search ranks by text alone, so five series sharing an episode title
  come back in arbitrary order — "Ozymandias" returns *Slacker Cats* above
  *Breaking Bad*.
- The browser resolves person images by display name over the live Wikipedia API
  on every request — a display-string lookup and a network call in the request
  path.
- Per-file in-place compaction. Compaction currently repacks globally, so file
  boundaries cascade and the diff is the whole store: +131 MB of history to
  reclaim 95 MB of tree, even after `repack --window=250`. Per-file compaction
  would make the diff pure deletions. Not urgent — squash-and-force-push
  reclaims everything, and `SyncStore` follows a rewritten history.

---

## Settled by measurement — do not re-litigate

**Import throughput, on an identical 300,000-page prefix.** 470 pages/s
recognising on the decode goroutine (`RunStream` calls its handler serially, so
every parser sat behind one core); 2,065 on the multistream reader; 2,921 with
lazy lead extraction (92% of pages are claimed by nothing, so eager extraction
did the work for twelve pages in thirteen and threw it away). Store 977 MB, then
409 MB with the wikitext gzipped — wikitext is 92.7% of the bytes. Lead and plot
are NOT duplicated wikitext: only 8 of 24,550 leads appear verbatim in their
source, because the markup is stripped. Do not "save space" by dropping them.

- **Ship no index.** 22 consecutive daily updates produced zero deleted lines;
  a client rebuilds in 60 s from a scan it needs for FTS anyway. `.idx` cost
  ~38% of daily growth and scaled with store size, not change size.
- **Tail-append beats hash placement**, 137–342× at matched file count: a day's
  changes land in 4 tail files instead of scattering across ~1,400.
- **Compaction is a net loss in git**: +131 MB history to reclaim 95 MB of tree.
- **int8 with a PER-DIMENSION scale** for vectors: 166 MB at 0.994 neighbour
  overlap. A global scale keeps about half. int4 0.905, int2 0.597.
- **Identity is the page_id, for every kind including people.** A Q-id is equally
  stable but only 63.5% of people have one, and it made identity depend on
  Wikidata for no gain.
- **Don't switch the payload to protobuf/flatbuffers.** JSON is ~40% of decode
  cost, zlib is more, and the whole decode is ~5 s across the cores extract
  uses. It would also cost a dependency and weaken the trained dictionary.
- **ColBERT won retrieval** (MRR 0.575 vs 0.198 lexical) but needs 13.25 GB and
  is server-only. Fusion loses to its better input; the store must stay int8.
- **Read the article, don't infer.** Four separate bugs came from assuming a
  convention the article states explicitly: time columns at a fixed offset,
  guessed season-article titles, `{{Episode list}}` vs its Lua module form,
  single-day headings.

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
- **Done: television in incremental ingest.** `filmstock update` merges a day's
  television changes into the series records the store already holds — a changed
  season article replaces that season, a changed series article takes the
  metadata and keeps the seasons other articles assembled. Ownership comes from
  Wikidata's stated P179/P4908 edge, never from the title.
  **Residual gap**: "List of X episodes" articles are attached in a full pass
  because the SERIES article states the link. An incremental pass only has that
  when the series article also changed; otherwise the source is counted
  unresolved rather than guessed, as the full pass does with its 1,705.
- **Compaction is index-bound, not data-bound.** gitdb record files compact
  beautifully — the diff reads as removed lines. The index shards do not: an
  entry holds an absolute offset, so removing one record rewrites every entry
  after it in that file. Measured: updating one record costs 3 changed lines;
  compacting the same store cost ~9,900, of which 8,875 were index entries whose
  offsets had moved. So `filmstock compact` is for after a schema change (it
  recovered 783.5 MB -> 420.7 MB, half the store being superseded versions), not
  for a daily job. Possibly a gitdb format question: length-based entries, or a
  compaction that preserves offsets, would make a compact diff as small as the
  data change.

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
