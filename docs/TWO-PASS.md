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
- the **raw wikitext**, so a parser fix never needs the dump again
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
| wikitext of recognised pages | 1,196,509 | 14–18 GB |
| person articles, all | 955,989 | 1.4 GB |
| films | 165,740 | 1.1 GB |
| television series | 61,342 | 0.4 GB |
| episodes | 572,847 | 0.3 GB |
| events, schedules | 53,240 | 0.03 GB |
| | | **~20 GB** |

On the NVMe, never shipped, against 2.3 TB free. Comparable to the resolver cache
before `wd_text` came out of it.

The parsed half is only ~3 GB; the wikitext is the bulk, and it is worth its
weight because it is what makes a parser fix cheap.

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

## Seasons become first class

A season is currently an anonymous struct nested in a series: number, episode
count, first and last aired, episodes. Wikipedia has considerably more, in two
complementary places, and neither is read today.

**`{{Series overview}}`**, on the episode-list page, gives every season at once
and — usefully — declares what its own columns mean:

    infoA = Rank   infoB = Rating   infoC = Viewers (millions)
    episodes1 = 25   start1 = 1994-09-19   end1 = 1995-05-18
    infoA1 = 2       infoB1 = 20.0         infoC1 = 30.1

Self-labelling matters because shows use the extra columns for different things.

**`{{Infobox television season}}`**, on each season article, gives what only that
season knows: `season_number`, `num_episodes`, `network`, `first_aired`,
`last_aired`, `starring`, an image, and prev/next links.

The season-specific cast is the most valuable of these, and the current model
cannot express it. `TelevisionSeries.Starring` is one flat list for a show's
whole run, so a fifteen-season series asserts that everyone who ever appeared was
in it throughout. Clooney was in ER for five seasons of fifteen. That is not
missing coverage, it is a modelling error being recorded as fact.

So Season gains: `PageID` (season articles are real pages with real ids, so a
season becomes addressable like everything else), `Rank`, `Rating`, `Viewers`,
`Network`, `Starring`, `Image`.

It is also the right home for the schedule join. A grid's slot — day, start, end,
network — and its Nielsen figures are properties of a season's arrangement, not
of an episode. Attaching a season's rank to each of its episodes would invent
precision the source does not have; episodes inherit by membership instead.

## Decisions to settle first

**Does the intermediate keep raw wikitext? — YES.**

I first said no, on the grounds that it meant 25 GB. That was wrong twice over.

It is not 25 GB: we recognise 1,196,509 of the dump's 25,792,234 pages, or 4.6%.
Keeping the wikitext of *those* is 14–18 GB, estimated from the 726 MB of
extracted plain text we already keep for 165,740 documents, scaled by document
count and by the ~1.5–2× that markup adds. On the NVMe that is nothing.

And the reason is stronger than insurance against a future field. **With the
wikitext held, a parser fix stops needing the dump at all.** Three tiers instead
of two:

    level 0   wikitext of recognised pages     re-parse costs minutes
    level 1   parsed entities                  re-export costs minutes
    records   published

    parser bug fixed   ──▶ re-parse from level 0
    record shape moved ──▶ re-export from level 1
    new dump           ──▶ re-import, full cost

Every parser bug found on 2026-08-26 — Lua-module episode lists, day ranges,
level-3 headings, uppercase PM, the dropped `Viewers` field — needed a
41-minute re-parse of 25 GB to take effect. Under this they are minutes, because
the 24 GB of pages that are not films, people, series or schedules never has to
be decompressed again.

It also keeps the door open for model training, which wants source text rather
than our interpretation of it, and does not want to re-derive it from a dump
each time.

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
