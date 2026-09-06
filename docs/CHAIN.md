# The release chain, and how it runs unattended

What a build is identified by, how the monthly rebuild and the daily deltas
converge without contradicting each other, and what the pipeline does when
something is wrong and nobody is watching.

This document exists because of a specific failure. On 2026-09-05 a monthly
rebuild from the 20260901 dump was about to be published onto a chain whose tip
was already at 20260902, and would have been again at 20260905. The full's
content stopped at 0901. Nothing in the code was in a position to notice: the
bridge diff would have been generated, verified against its own content hashes,
and published — and it would have walked every consumer *backwards*, silently,
until the next full a month later.

Two things were wrong, and both are structural rather than clerical.

---

## 1. Identity: a build id is a date, and dates collide

The README is explicit that identity is never a display string: keying films on
their title merges the fifty different series that render as *Big Brother*, and
keying people on their name merges distinct people. Every record in the database
is keyed on `page_id` or Q-id for exactly this reason.

A build id is `YYYYMMDD`. The monthly full derived from the 20260901 dump and
the daily built from the 20260901 adds-changes dump are two different artifacts
that both render as `20260901`. The catalog can hold one of them. This is the
same bug the record layer was designed to avoid, one layer up.

It is not a naming inconvenience. It is what forced the question "which id
should the full take?", and that question has no good answer — every candidate
either collides with a published daily, or dates the build as something it is
not.

### The change

A build id becomes opaque and monotonic, allocated by the publisher rather than
chosen by the caller. The date facts become attributes:

| field | meaning |
|---|---|
| `id` | opaque, monotonic, unique. Never parsed for meaning. |
| `dump` | the Wikimedia dump the content derives from |
| `through` | the last adds-changes day applied — the build's **content day** |

`through` is the field that has been missing. The intermediate has always known
it (`incr_through`, read by `lastAppliedDay` in `catchup.go`), but it never
reached the catalog, so the chain was ordered by *label* while correctness
depended on *content day*, and nothing checked that the two agreed.

Consumers that want to show a date show `through`. Nothing orders builds by
parsing an id ever again.

---

## 2. Ordering: the diamond, and why dominance settles it

The pipeline has two production paths that must converge into one chain:

```
    full(dump D) ────────────┐
                             ├──> ?
    tip = full + deltas ─────┘
```

Asking "which is the source of truth, the full or the deltas" has no answer,
because neither one dominates:

- **A full is authoritative for existence.** Page deletions arrive only in a
  full; no adds-changes dump reports them.
- **The deltas are authoritative for recency.** They carry edits made after the
  dump was cut.

So a full published against a tip that has advanced past it is not merely stale
— it is *missing information the chain already had*, and adopting it destroys
that information. That is the 2026-09-05 failure exactly.

### The invariant

A full whose intermediate has replayed **every delta the tip contains** holds
both properties: the full's page set *and* the deltas' recency. It is a superset
of both inputs. It dominates by construction, and no adjudication is required —
nothing is being reverted because there is nothing the other branch knows that
it does not.

> **A build may only be published when `through(new) >= through(tip)`.**
> `publish` refuses otherwise.

This is not a policy choice about who wins. It is the condition under which the
question does not arise. Violating it is the only way to produce a join where
neither side contains the other, which is the only way a bridge can revert.

`through(new) == through(tip)` is the case worth aiming for: the two branches
then describe the same day by different routes, and the diff between them is
pure drift.

---

## 3. The gate: structural, not a threshold

`bridge_statements` is described as the pipeline's integrity meter: the diff
between [previous full + every daily] and [this full, rebuilt from scratch].
The obvious unattended gate is a ceiling on it — refuse a bridge over N
statements — and it does not work. It is worth writing down why, because N is
tempting every time this is read.

A bridge's size is dominated by *legitimate repair*. Adds-changes dumps are
retained about 42 days, so any day the daily job missed for longer than that can
never be applied. `20260802`–`20260817` aged out exactly this way, and the
20260901 full is what closes that gap: its bridge carries sixteen days of edits
the chain never saw. No statement count distinguishes "repairing sixteen missing
days" from "reverting the chain to a stale dump" — they are the same magnitude
and the same shape. A ceiling high enough to admit the first admits the second;
one low enough to catch the second blocks every release that does real work.

A tuned constant here is a fudge factor standing in for not knowing what the
diff contains. So know what it contains.

### What is actually enforced

The failure being guarded against — a full whose content predates the tip —
manifests as records *disappearing* for pages that still exist. That is
checkable exactly, against the page set of the dump the full was built from,
with no constant to tune:

> A record in the tip and absent from the new build is legitimate **iff** its
> `page_id` is absent from the new dump. Otherwise it is a bug.

Zero unexplained removals. A deleted article, a merged redirect, a page moved
out of main namespace all satisfy it. A rewind fails it — and so do a parser
regression that stops recognising a template and a truncated import, which are
worth catching for their own sake and which no size threshold would have caught
either.

The page set is the resolver cache for that dump: `wiki_qid` is rebuilt
DROP/CREATE from the dump's own multistream index, so it is that dump's page set
and not a stale copy of an older one.

Checked tables are the ones whose identity *is* a page: `movies`,
`television_series` and `events` are keyed on `page_id`, and `people` carries
one. Seasons and episodes are deliberately excluded — their ids are content
hashes, most seasons have no article of their own, and an episode row vanishing
because someone reformatted a "List of episodes" table is an ordinary edit.

`bridge_statements` is still published, because it is worth seeing. It is a
measurement, not a test.

---

## 4. Running unattended

### One entry point

`scripts/run.sh` is the whole schedule:

```
40 3 * * *  /tank/mediadb/filmstock/scripts/run.sh
```

It runs the day's incremental builds, then rebuilds from a fresh dump if one has
appeared. Most days the second half finds nothing and says so in a line.

They are sequenced rather than given a cron line each because they are not
independent: a monthly rebuild may only supersede the tip once it has replayed
every delta that tip holds (§2), so it wants the day's dailies to have landed
first. Sequencing makes that ordering a property of the schedule instead of a
race between two timers.

`run.sh` takes its own non-blocking lock so two invocations never overlap — a
rebuild runs for hours and tomorrow's cron must not start a second one on the
same dump. The halves keep their own locking underneath and nothing is nested:
`daily.sh` takes the build lock per day and releases it on exit, and
`monthly.sh` takes the same lock only for its short convergence-and-publish
phase. A skipped run costs nothing, because `catchup` knows which days it is
behind and the monthly resumes from its phase markers.

Either half failing does not stop the other being attempted: they fail for
unrelated reasons — a late adds-changes dump versus a full dump's jobs
finishing — and one being broken is a poor reason to skip the other. Both
outcomes are reported and the exit status is nonzero if either failed, so cron
mails once with the whole picture.

### The discipline both halves share

`daily.sh` already establishes it: one flock, refuse to publish from a dirty
tree, preflight everything that could fail halfway, fail-stop with the diagnosis
in the output, every step re-runnable. The monthly is the same shape with two
additions.

Nothing in either half may pin a date. `daily.sh` used to hardcode the full dump
set it resolved against, which the first promotion would have silently
invalidated; it now reads the dump path out of the intermediate's own `source`.
And it compared the intermediate's content day against the tip's *id*, which
stops meaning anything the moment an id is not a date — it compares `through`
now.

### Resuming, and what a marker means

Phase completion is recorded with an explicit marker written only after the step
returns successfully, and never inferred from a file existing. A crashed import
leaves a large, well-formed, incomplete intermediate that `-s` cannot tell from a
finished one; resuming on that would converge, export and publish from a
truncated corpus. The removal guard (§3) would catch it, but only after hours of
wasted work, and only for the page-keyed tables.

### The dump is not ready because the calendar says so

The monthly cannot assume a dump exists on a date. It polls
`dumps.wikimedia.org/enwiki/<date>/dumpstatus.json` and proceeds only when the
four jobs this pipeline consumes report `done`:
`articlesdumprecombine`, `articlesmultistreamdump`, `pagepropstable`,
`imagetable`. The history and pagelogs jobs lag by days and are not consumed —
waiting on overall completion would delay every monthly for no reason.

Not ready is not a failure. It retries.

### We do not control when a dump appears

A dump is named for the day its content was snapshotted, not the day it is
published. `enwiki-20260901` finished its article jobs on 2026-09-03; nothing
promises three days. It could as easily be thirty — a dump dated 10/01 finishing
on 10/30 — and that is not an error condition, it is Wikimedia's schedule.

The consequence is that a rebuild's content always starts at the dump's date and
has to be carried forward to the published tip by replaying the adds-changes
dumps in between. The later a dump lands, the more days that is. If any of those
days is unavailable, the rebuild can never satisfy §2's invariant, and `publish`
refuses it — correctly, and at the very end of several hours of work.

Two things follow.

**Retention here is the insurance, and it is cheap.** Wikimedia keeps
adds-changes dumps about 42 days; that is their budget, not ours. Once a day is
downloaded it is ours to keep, and these files are the only thing that can carry
a late rebuild onto the tip. Keeping 45 days made our margin the same as the
server's, which is to say none. At ~850 MB/day, half a year is ~155 GB against
37 TB free, so `FILMSTOCK_KEEP_INCR` now defaults to 180 days. A dump a month
late then converges from files already on disk and the lateness is a non-event.

**A doomed rebuild is refused in the first minute, not the last.** Before
fetching ~27 GB, the monthly enumerates the days between the dump's date and the
tip's content day and checks each is held locally or still on the server. If any
is gone from both, it stops and names them. The answer does not change by
working for four hours first, and a run that fails early with the missing days
listed is the same outcome made legible.

Neither of these repairs a gap that has already opened. Nothing can: if the days
between a dump and the tip are genuinely gone, that dump is unusable and the
pipeline waits for a later one while the dailies carry on working. What they do
is make the situation nearly unreachable, and unmistakable when it happens.

### The long work holds no lock

The monthly's expensive phases — download, qidmap, import — write only files
nothing else reads: a new dump directory, a resolver copy, a new intermediate.
They take hours and hold no lock, so dailies continue to land throughout.

Only convergence and publication are serialised, under the same `.daily.lock`
the daily job takes:

```
(no lock)  fetch dump; rebuild qidmap; import -> intermediate-<new>.db
(lock)     catchup intermediate-<new> to the tip's `through`
           export; post-passes
           measure drift; gate
           publish full
           promote intermediate-<new>.db to the canonical path
(unlock)
```

The promotion is what makes the next daily use the new intermediate, and it is
a rename under the lock — so there is no window in which a daily could apply a
day to the intermediate that is about to be replaced.

A daily that cannot take the lock exits 0 and is picked up by the next run;
`catchup` already knows which days it is behind, so a skipped day is not a lost
one.

### Every failure leaves the chain where it was

The monthly builds into new files throughout. A crash at any point before the
publish leaves the old intermediate, the old resolver and the published chain
untouched, and the run is repeatable from the start. This is worth preserving
deliberately: it is the reason a monthly can be retried by a timer rather than
by a person.

### The one thing that does not self-heal

Adds-changes dumps are retained for about 42 days. If the monthly has not
succeeded within that window, the gap can no longer be closed by dailies at all,
and the chain can only be repaired by a full rebuild.

So the health check is not "did last night's run pass" but:

> `today - through(tip)` and `today - through(latest_full)`

Both are alertable, with the second escalating well before 42 days. Under
autonomy this is the check that matters most, because it is the only failure
that gets *harder* to fix the longer it goes unnoticed. Every other failure mode
here is a retry.

---

## 5. What a consumer downloads

Two requirements set the shape of the published tree, and they pull against each
other:

1. Someone installing fresh in three years must not download a thousand daily
   diffs.
2. Someone already following must not download a full database every month.

Monthly fulls satisfy (1) on their own: the newest full is never more than a
month old, so a fresh install is one full plus at most ~30 dailies. The bridge
satisfies (2): a follower crosses to each new full by applying a diff rather
than refetching ~1.3 GB — which is the entire reason the bridge exists, and the
reason invariant §2 matters commercially as well as correctly. A same-`through`
bridge is pure drift and therefore small. A bridge that carries real content is
still a diff and still cheaper than a full, but it is no longer the cheap
crossing the design intends.

Neither addresses the case between them: a consumer who held a build and was
away for six months. Today they must walk six bridges and roughly a hundred and
eighty dailies, or abandon the chain and refetch a full. That case does not
improve with time; it degrades linearly.

### Rollup patches

Publish cumulative diffs alongside the daily ones — a weekly patch spanning
seven days, a monthly patch spanning full to full. They cost a `sqldiff` run
each and some hosting; they are not new information, only new routes through the
same information.

The hop count then stops depending on how long someone was away:

| consumer | today | with rollups |
|---|---|---|
| fresh install | full + ≤30 dailies | unchanged |
| daily follower | 1 daily; 1 bridge at the month | unchanged |
| away six months | 6 bridges + ~180 dailies | ~6 monthly patches |
| away three years | ~1000 hops, or refetch | ~36 monthly patches, or refetch |

### Let the consumer choose the route

Once more than one route exists, the catalog should stop prescribing one. Every
patch already has a known size in its manifest, so publish the available edges —
`from`, `to`, `path`, `bytes` — and let the updater pick the cheapest path to
the tip by total bytes, against the cost of the newest full.

This needs no thresholds and no policy constants, and it produces the right
answer in every row of that table without being told about any of them: a fresh
install finds the full cheapest, a daily follower finds one patch, a returning
consumer finds the rollups, and a consumer away so long that no path beats
refetching gets exactly the fallback the updater already has. It also degrades
correctly when a route is missing — a pruned old full or an aged-out patch
simply removes an edge, and the search routes around it instead of failing.

The chain becomes a DAG, which is a real cost: the updater runs a shortest-path
search rather than following a line. It is a few hundred edges and the weights
are bytes, so the search is not the hard part — the discipline is that every
edge must be verified the way patches are verified today, applied to a copy of
its base and refused unless the content hash reproduces. An unverified route is
worse than a missing one.

Pruning becomes a policy question the search makes safe to answer: dropping an
old full or old dailies removes edges, and as long as *some* verified path
remains from any still-supported build, no consumer is stranded.

### As built

- `publish -rollups 7,30` emits, alongside the parent patch, a patch spanning
  7 and 30 builds back, named `<db>.from-<id>.patch.sql.gz` so several routes
  live in one build directory without colliding. Each is applied to a copy of
  its base and refused unless it reproduces the target's content hash — the
  same proof the parent patch has always carried.
- `publish -keep-tips 31` retains that many recent builds' databases in the
  work directory. A rollup can only be built against databases still on disk;
  a span whose source has been pruned is simply not offered, which costs a
  longer path and never correctness.
- The catalog records `edges` (`from`, `suffix`, `bytes`) per build and `bytes`
  on each full — the full road's price, which every route is weighed against.
- The updater's `cheapest` replaces the old backwards walk with a byte-weighted
  search over those edges, seeded from what is held and from every full.
- `builds -backfill-edges <root>` prices the routes of builds published before
  the field existed, from the patch files already on disk. Priced, not left at
  zero: to a path search zero is not "unknown" but "free", and a free edge wins
  every comparison — an unpriced legacy chain beside a priced full would send a
  fresh install walking every legacy patch instead of taking the full.

## 6. Signalling a fresh start

Everything above assumes continuity: a full bridges onto the lineage it
supersedes, which is exactly what stops a follower re-downloading a database
every month. Sometimes continuity is the wrong claim — a schema change old
patches cannot express, or a chain discovered to be wrong — and the consumer
must abandon what it holds.

Today that would be discovered by *failing*: the updater takes a route, the
result does not verify, and it falls back to the full. A break inferred from a
failure is indistinguishable from a bug in the patch, and after a schema change
the old patches may not be safe to attempt at all.

So the break is declared, not inferred. Every build carries an `epoch`, and
**routes never cross epochs**. `publish -full -fresh -reason "…"` opens a new
one: no bridge is emitted, no edge leads out of the old lineage, and every
consumer takes the full road on purpose. The updater says so in as many words,
naming the reason, rather than logging a verification failure.

This is orthogonal to `-full`, and deliberately awkward to reach for. A monthly
full continues the lineage; `-fresh` costs every consumer a full download, so it
refuses without `-reason` — the one thing a consumer gets in return for that
cost is being told why — refuses on a daily, and refuses a `-reason` given
without it.

The first epoch is 0, which is what every build published so far already reads
as, so nothing needs migrating.

### Bumping `ContentHashVersion` is an epoch break

Content hashes are versioned and the version is part of the hashed bytes, so a
client with older rules mismatches every build published under newer ones. That
is not a degradation, it is a total loss of verification: the patch road refuses,
and the full road downloads the whole database, rebuilds its indexes, re-hashes
and refuses that too. The client fails safe and never moves again.

Which is exactly the definition of "nothing held can be carried forward". So
changing `ContentHashVersion` requires publishing the next full with
`-fresh -reason "content hash vN"`.

A consumer must also be told which of the two it is. Manifests state
`content_hash_version`, and the updater now reads it before doing any work: a
build published under rules it does not understand is refused immediately,
naming both versions and saying to upgrade, rather than after a wasted download
with an error that reads like a corrupt build.

File hashes cannot do this job and are not asked to. A consumer that applies
patches holds the same facts as a freshly built file in different bytes —
different page layout, freelist, AUTOINCREMENT counters — so `sha256` verifies a
*download* and `content_hash` verifies a *state*. That distinction is why
identifying which build a local copy actually holds is possible at all.

## Migration

The 17 existing entries keep their ids — they are already unique, and rewriting
published identifiers would break held builds. They gain `through` (equal to
their existing id, which is what those ids meant) and `dump`. New builds take
opaque ids. The updater orders by `through` and follows `parent`/`bridge` edges
as it does today; it never parses an id, which is the only behaviour change it
needs.
