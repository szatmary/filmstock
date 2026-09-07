# Franchise / collection coverage

Investigated 2026-09-06 against dump 20260901. No code changed — a full chain
rebuild was in flight. This records the diagnosis so it does not need
rediscovering; the measurements cost several multi-minute scans of an 8 GB
database under I/O contention.

## Is there a Wikipedia standard for "collections"?

Not in Wikipedia proper. `Template:Infobox film` has no franchise or collection
parameter, so an infobox parser will never see one. Wikipedia only carries soft
signals: categories (`Category:Marvel Cinematic Universe films`), navboxes, and
prose list articles. All are display strings and all get renamed.

The machine-readable source is Wikidata:

| Property | Meaning | Extracted today |
|---|---|---|
| P179  | part of the series | yes (`wdedges.go`) |
| P156 / P155 | followed by / follows | yes |
| P4908 | season | yes |
| **P8345** | **media franchise** | **no** |
| P361  | part of (generic) | no |

Canon vs. non-canon (Star Wars Legends, say) is editorial prose only. It is not
modelled anywhere machine-readable. Do not try to derive it.

## We already do this, without hand curation

`internal/build/seriesindex.go` builds `franchises` / `franchise_members` from
P179. It works. The problem is only coverage.

Last completed run:

```
702 franchises
3,346 movie members        of 166,220 movies      = 2.0%
  704 television members   of  65,205 television  = 1.1%
```

Where it fires it is clean: MCU (Q642878) returns 47 correct members,
Monsterverse (Q21527684) returns 6.

## Why coverage is ~2%

### 1. P179 does not chain upward to the franchise

Verified in `resolver-ext-20260901.db`:

- `Star Wars (film)` -> `Star Wars original trilogy` (Q25540859) and Q22092344.
- `Star Wars original trilogy` has **no parent row at all**. The chain stops.
- So it never reaches Q462 (`Star Wars`), which has just 16 members — all novels.
- `Wizarding World` (Q30739117) has exactly 1 member, a TV series.

Walking P179 transitively therefore does not fix this: the edge to the top-level
franchise is a different property, **P8345**, which `wdedges.go` does not extract.
That is the missing pass, and it is cheap — one more property in a scan already
made over `all.json.bz2`.

P361 also encodes some of these, but it is the sloppy generic one and
over-clusters (it will pull in things like award categories). Add P8345 first,
measure, then decide whether P361 earns its place.

### 2. A franchise is dropped unless it has its own enwiki article

`seriesindex.go` joins `wiki_qid fq ON fq.qid = s.series_qid` so it can key the
franchise by `page_id`. Any Wikidata-only series item is discarded — including
Q22092344 above.

```
8,242  series with >=2 enwiki-article members
6,032  of those have their own article
2,210  do not                                -> ~27% of clusters lost
```

(Media-wide, not film/TV-only; there was no built `index.db` to join `movies`
against at the time.)

This is the same mistake as the general rule elsewhere in this codebase: a
perfectly good Q-id thrown away for lack of a display title. Key franchises by
`qid`, and fall back to `page_id` only when an article happens to exist. An
unnamed cluster is still a valid cluster and can be named later from its members.

## Before investing

Most of 166k films genuinely belong to no franchise, so 100% was never the
target and 2% is not straightforwardly "98% broken". Measure against a
known-complete subset — e.g. what fraction of films that have a `sequels` row
also land in a franchise — rather than against the full corpus. Fixing (2) alone
only moves 2.0% to roughly 2.5%; **P8345 is the larger lever.**

## Operational note

`rebuild.sh` invokes `$H/filmstock/filmstock` for every step across many hours
(import, export, index-external-ids, index-series, publish, then daily.sh per
held day, then monthly.sh). Editing `.go` sources during a run is harmless — the
running process holds its inode and Go links to a temp then renames — but
`go build` swaps the binary the script reaches for on its *next* step, producing
a published chain built from two code versions with the schema changing partway
through. Since `publish -full -sqldiff` is what the chain is keyed on, that is
much worse than the bug being fixed. Check `ps -eo pid,pgid,etime,cmd | grep
filmstock` first; if a chain run is live, build in a separate worktree.
