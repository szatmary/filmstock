# How filmstock data is produced

What gets downloaded, what is pulled out of it, how the pieces are joined, and
what ends up in the database. This is the data-level account; how records are
encoded and stored is in the gitdb package documentation.

---

## 1. What is downloaded

Five files, from `dumps.wikimedia.org`. Four are the periodic full build; the
fifth is the daily increment.

| file | size | what it is |
|---|---|---|
| `enwiki-YYYYMMDD-pages-articles-multistream.xml.bz2` | ~25 GB | the current text of every English Wikipedia article |
| `enwiki-YYYYMMDD-pages-articles-multistream-index.txt.bz2` | ~270 MB | `offset:page_id:title` for every page |
| `enwiki-YYYYMMDD-page_props.sql.gz` | ~440 MB | page properties, including each page's Wikidata item |
| `latest-all.json.bz2` (wikidatawiki) | ~95 GB | every Wikidata entity, with its claims and sitelinks |
| `enwiki-YYYYMMDD-pages-meta-hist-incr.xml.bz2` | ~800 MB/day | one day of adds and changes |

Only the English Wikipedia dump is a source of *content*. The other three exist
to answer questions the article text cannot answer about itself:

- **page_props** gives each article its Wikidata item, which is how a person's
  Q-id is known.
- **the multistream index** gives every title its `page_id`, which is the
  identity everything is keyed on.
- **Wikidata entities** state relationships that Wikipedia's own prose does not
  — specifically which series a television season belongs to.

The adds-changes dumps are published daily and retained for about 42 days. Past
that window the only way to catch up is a new full dump.

---

## 2. What is extracted, and how the sources combine

### Phase 1 — Wikidata relationships

One streaming pass over the 95 GB entity dump, keeping two claim types:

- **P179** *part of the series* — season → series (*Lost, season 1* → *Lost*)
- **P4908** *season* — episode → season (*Homecoming* → *Lost, season 1*)

Roughly 1.07M P179 edges and 179k P4908 edges survive. Everything else in the
entity dump is discarded.

This phase exists solely for television. A season article names its own season
and, very often, not its series; an episode names only its season. The only
trustworthy statement of the relationship is Wikidata's, so the alternative is
matching on titles — which silently merges different shows that share a name.

### Phase 2 — the identity map

`page_props` and the multistream index are joined into one table:
`title → (page_id, Q-id)`, about 10.3M rows.

This is what lets the P179 edges — which are expressed in Q-ids — be resolved to
`page_id`s, so the join between a season and its series never compares strings.

Phases 1 and 2 together are the *resolver cache*. It is build-time only, is
never published, and is reused across runs: about 1 GB, and roughly 46 minutes
to build from scratch.

### Phase 3 — the article pass

One streaming pass over all ~25.8M pages. Only main-namespace pages are
considered. Each is classified by which infobox template it carries, and a page
may match more than one test.

**Films** — an exact `{{Infobox film}}`. The match is exact rather than a prefix
because `{{Infobox film awards}}` and `{{Infobox Film festival}}` both begin with
"Infobox film"; matching by prefix once put 2,418 award ceremonies and 1,647
festivals into the film index with no cast and no year.

**Television** — `{{Infobox television}}` for a series, `{{Infobox television
season}}` for a season, and `{{Episode list}}` rows wherever they appear, which
is usually on a separate *List of X episodes* article rather than on the series
page.

**Events** — `{{Infobox film awards}}` for a ceremony, `{{Infobox Film
festival}}` for a festival. Both capitalisations are matched, because Cannes
capitalises the template and the Academy Awards do not.

**People** — any of twelve biography infoboxes: `Infobox person`, `actor`,
`musical artist`, `writer`, `artist`, `comedian`, `model`, `architect`,
`academic`, `officeholder`, `military person`, `sportsperson`. Note there is no
`Infobox director` or `Infobox producer` in that list — those people are
recognised through one of the general templates, usually `Infobox person`.

From each matching page the structured fields of the infobox are read, plus two
pieces of prose: the **lead** (the opening paragraphs, used as an overview) and
the **plot** section where one exists.

### How people are found

This is the part that combines sources rather than reading one.

People are discovered from **credits**, not from biographies. Every linked name
in a film's director/producer/writer/cast/music/cinematography/editing fields,
and in a series' creator/starring/composer/presenter/narrator and the rest, is
noted as a person. A name that is *not* linked is ignored: an unlinked "John
Smith" has no stated identity, and attaching him to whoever holds that article
title would be an invention.

Biographies are collected in the same pass but cannot be attached on sight,
because a person's article may be read thousands of pages before or after the
film that credits them. They are held aside and joined at the end of the pass by
article title.

The result is that a person record carries their identity and credits always, and
biographical detail only when their article was itself in the dump and was
recognised as a biography. About 53% have one.

### Identity

Every record — film, series, event, person — is keyed by its enwiki `page_id`.

For people this is taken from their own article where the biography was parsed,
and from the identity map otherwise. The two sources matter because the identity
map only covers pages that have a Wikidata item, so an article without one is
invisible to it; the dump is what covers those.

One case has no canonical identity: a credit whose link points at an article
that does not exist. There is no `page_id` and no Q-id to be had, so such a
person is keyed by a hash of the link target — a display string, which two
different people can share. About 77,000 people, a third of the total, holding
around 10% of credits. Every extract reports the count so the exception stays
visible.

### Phase 4 — the search index

The record store is walked once and a SQLite index is built from it: titles,
years, credits, episode lists, and full-text search. It is derived, not
published, and rebuilds in about a minute.

---

## 3. What comes out

| | count |
|---|---|
| films | 166,073 |
| television series | 61,342 |
| television episodes | 560,484 |
| people | 234,125 |
| credits | 1,403,676 |
| award ceremonies and festivals | 4,705 |

**Films** carry title, page_id, overview and plot, genre, release dates,
director, producer, writer, cast, music, cinematography, editing, production
companies, distributor, source material, country, language, runtime, budget,
gross, and cover image.

**Television series** carry the same shape plus a season and episode structure;
each episode carries its number in the season and overall, title, air date,
director, writer and summary.

**People** carry name, page_id, Q-id where known, article link, and — when their
article was present — birth and death, occupation, nationality, years active,
image, and the lead of their article.

**Events** carry the ceremony or festival, its edition and date, hosts, venue,
and the films named in it.

---

## 4. Keeping it current

Two rhythms.

**Daily.** One adds-changes dump is applied: every changed article is re-derived
and compared against the stored record, and only genuine differences are written.
Roughly 8–10% of a day's edited articles touch something in this database, and of
those about 85% re-derive to byte-identical records — a typical day changes
around 1,400 records out of 467,000.

**Periodically, a full rebuild.** The daily path is deliberately narrow: it
updates what is already known. It does not currently discover people who appear
for the first time in a newly added film, so those arrive with a credit but no
record until a full extract reconciles them — about 21 a day. A full rebuild
takes about 20 minutes when the resolver cache is warm, and about 70 minutes when
it must be built from the Wikidata dump.

---

## 5. What is not done

**Deletions.** A page that disappears from Wikipedia is not removed here. The
adds-changes dumps say what was added or changed and never what was deleted, so
this can only be reconciled by a full pass.

**People discovered incrementally.** As above — new credits create index entries
without store records, and they carry no canonical identity until a full rebuild.

**Wikidata freshness.** The resolver cache is rebuilt only with a full extract,
so television relationships stated after it was built are not yet known. The
effect is limited to seasons that fail to attach to their series.
