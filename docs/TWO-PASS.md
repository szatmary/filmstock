# Two-pass import/export

A design, not yet built.

## Why

The daily path cannot add a person. `applyBiography` only ever updates someone
already in the store, because a daily dump contains one day of edits and a new
film's cast were not edited that day — their articles sit unchanged in a dump we
are not reading. So a film added today brings a cast list, the index makes credit
rows from it, and any genuinely new person gets no record, no `page_id`, and no
biography. Measured: 478 such people over 23 daily updates, none with a canonical
identity, and never reconciled without a full re-extract.

That is the immediate reason. It is not the largest one.

**A schema change currently costs a full re-parse.** Adding a field, fixing a
parser, changing a title rule — each means streaming 25 GB again, 41 minutes with
the resolver cache warm and 70 without. On 2026-08-26 alone six full extracts ran
for changes that touched how records are *shaped*, not how the dump is *read*.
Every one of those re-derived parses that had not changed.

The dump is read to produce parsed entities. Records are produced from parsed
entities. Those are different jobs and they change at different rates: the first
changes when Wikipedia's markup changes, the second whenever we learn something
about what consumers want. Fusing them means paying the first cost every time the
second question is asked.

## Shape

```
import    dump ──▶ intermediate      parse only; keep everything; no joins
export    intermediate ──▶ records   resolve references, filter, emit
update    day ──▶ intermediate, then re-export what changed and what refers to it
```

The intermediate is build-time infrastructure. It is never published, exactly
like the resolver cache, and can be rebuilt from a dump at any time.

### Pass 1 — import

Stream every main-namespace page once. For each page that any recogniser claims,
store what was parsed and nothing derived:

- `page_id`, title, kind
- the **complete** infobox map, values unparsed
- the lead, and the plot section where there is one
- every link target the page states, with the field it came from

No cross-referencing. No deciding whether a person is credited, whether a season
belongs to a series, whether a film's cast link resolves. Those are export's job
and every one of them needs the whole corpus, which is precisely what a single
streaming pass does not have at the moment it reads a page.

**Keep every person, not the credited ones.** We currently see 955,989 person
articles and keep the 234,778 that something credits. The discarded 721,211 are
the reason a daily update cannot resolve a new cast member: the information was
in the dump and we threw it away because, at that moment, nothing had asked for
it. Storing all of them is 1.4 GB.

### Pass 2 — export

Read the intermediate, resolve, filter, emit the published records. This is where
every decision that is currently baked into the streaming pass moves to:

- which people are entities and which are only credits on a film
- whether a season attaches to a series
- what a display title looks like
- which fields a record carries, and whether absent means omitted

All of them become re-runnable in minutes against a stable input.

### Update

Apply the day's pages to the intermediate as an upsert by `page_id`, then
re-export what changed **and what refers to it**. A new film means exporting a
person who did not change, because their credit list did.

## Size

| | | |
|---|---|---|
| person articles, all | 955,989 | 1.4 GB |
| films | 165,740 | 1.1 GB |
| television series | 61,342 | 0.4 GB |
| episodes | 572,847 | 0.3 GB |
| events, schedules | 53,240 | 0.03 GB |
| | | **~3.1 GB** |

Uncompressed, on the NVMe, never shipped. Smaller than `index.db` plus the
vectors, and an eighth of what the resolver cache was before `wd_text` came out.

The intuition that "we must save all people, so it will be a large database" is
right about the first half and wrong about the second: the volume is in raw
wikitext, and the intermediate stores parsed structure instead.

## What this fixes beyond people

**Deletions.** A page absent from a fresh import but present in the previous one
was deleted. Currently impossible incrementally, because adds-changes dumps state
what changed and never what went away.

**Reversible decisions.** Whether redlinked names are person records, whether
records use `omitempty`, how a title displays — each becomes an export flag
rather than a 41-minute re-extract. Both of those are open questions today
precisely because changing one's mind is expensive.

**Determinism.** Export from a stable intermediate removes the arrival-order
nondeterminism that leaves ~339 records varying between otherwise identical runs.

**Cheap bug fixes.** Every parser bug found on 2026-08-26 — the Lua-module
episode lists, day ranges, level-3 headings, uppercase `PM` — needed a full
re-extract to take effect. Under this split, the ones that change *parsing* still
need a re-import, but the ones that change *shaping* do not.

## Decisions to settle first

**Does the intermediate keep raw wikitext?**

Against: it is 25 GB and the dump already exists on disk. For: a field nobody
anticipated needs a re-import without it.

Recommendation: no. Store the complete infobox map — every key, values
untouched — plus lead, plot, and all stated link targets. Film and series records
already carry `Raw map[string]string` for exactly this reason, so the pattern is
established. A future field that needs something outside that set is rare enough
to be worth a re-import.

**What triggers a re-export?**

The intermediate must record the reference graph, not only entities, or an
incremental export cannot know what went stale. Adding a film has to mark its
cast for re-export. This is the part with real design risk: get it wrong and
incremental export silently emits a stale record, which is the same class of
failure as everything else that went wrong this month — wrong quietly, not
loudly.

**Storage format.**

SQLite. Random access by `page_id`, joins for the reference graph, and the
project already depends on `modernc.org/sqlite` with no cgo. The resolver cache
is the precedent.

## Cost

The parsers do not change. `buildFilm`, `buildMovie`, `buildTelevisionSeries`,
`buildEvent`, `buildBiography`, `buildSchedule` all move behind `import`
unmodified — they already take a page and return a parsed entity. What changes is
orchestration: what gets held, what gets joined, and when.

The risk is concentrated in the reference graph and in incremental export. Both
deserve tests that assert staleness is detected, on the evidence that silent
wrongness is this codebase's characteristic failure.
