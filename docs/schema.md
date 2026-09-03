# The filmstock schema

Three SQLite files. Open them with any SQLite library, in any language — the
Go package here is for fetching and updating them, not for querying them.
Nothing below needs it.

## Reading them together

The files are separate so a consumer downloads only what it uses, but they
join on shared ids. Attach them and query across:

```sql
ATTACH DATABASE 'filmstock-text.db'    AS text;
ATTACH DATABASE 'filmstock-vectors.db' AS vec;

SELECT m.title, m.year, t.plot
FROM movies m
JOIN text.movie_text t ON t.id = m.id
WHERE m.year = 1954;
```

## Identity

**Every id is the English Wikipedia page_id of the thing's own article.** Not a
row number, not a hash, not a title. It is stable across builds, so a consumer
may store it, and it is the join key everywhere: `movies.id`,
`television_series.id`, `events.id`, `text.movie_text.id`,
`vec.vectors.id` all mean the same article.

Two exceptions, both deliberate:

- **People with no article.** A credit naming someone who has no Wikipedia
  page still counts, so they get a row whose id is derived from their link
  target with bit 31 set, keeping it clear of real page_ids. Their
  `page_id` column is 0.
- **Rows for things that are not articles** — seasons, episodes, schedule
  slots — get an id derived from their content (series, season number, title
  and so on). It is stable between builds for the same content, which is what
  lets daily updates ship as small diffs, but it re-keys if the content it is
  derived from changes.

## Conventions

- **List-shaped columns are display strings**, joined with " · " (space, middle
  dot, space): `starring`, `genre`, `country`,
  `language`. Split on that separator if you need the elements. They
  are for showing and for LIKE; for anything relational, join `credits`.
- **Titles are stripped of Wikipedia's disambiguator** — "Batman (1989 film)"
  is stored as `title` = 'Batman', with the original in
  `wiki_title`. Since titles are not unique (11,925 titles are shared
  by 31,934 films), display the year alongside, and never key on a title.
- **Absent means absent.** An empty string or NULL means Wikipedia did not
  state it, never that the value is zero or unknown-but-guessed.
- **Dates are strings**, as stated: usually `YYYY-MM-DD`, sometimes
  less precise. They sort correctly as text when fully specified.

## The full-text indexes ship EMPTY

`movies_fts` and the other `*_fts` tables are declared but
carry no rows. They are derived data — 181 MB of it — that rebuilds locally in
seconds, and any daily update would stale them anyway. Rebuild them once after
downloading, then again after each update:

```sql
INSERT INTO movies_fts(movies_fts)              VALUES('rebuild');
INSERT INTO television_fts(television_fts)      VALUES('rebuild');
INSERT INTO television_episodes_fts(television_episodes_fts) VALUES('rebuild');
INSERT INTO events_fts(events_fts)              VALUES('rebuild');
INSERT INTO people_fts(people_fts)              VALUES('rebuild');
```

They are FTS5 external-content tables over the base tables, tokenized
`trigram`, so they match substrings and tolerate typos when combined
with your own ranking. The Go updater does this rebuild for you.

## Credits

`credits` is the relational join between people and works, and the one
place to ask "what did this person do" or "who worked on this":

```sql
SELECT p.name, c.role
FROM credits c JOIN people p ON p.id = c.person_id
WHERE c.work_id = 31371 AND c.work_type = 'movie';
```

`work_type` is `movie`, `television` or
`event`, and `work_id` is that work's id. A person credited
under an old article title still resolves, because `person_alias` maps
every link target to the person it redirects to.

## External identifiers

`external_ids` carries IMDb, TMDb, TVDB and Rotten Tomatoes ids from
Wikidata — join keys only, no ratings or content from those services:

```sql
SELECT value FROM external_ids
WHERE id = 31371 AND kind = 'movies' AND source = 'imdb';
```

## Regenerating this document

```
filmstock schema -db bucket/<build>/filmstock.db -out docs/schema.md
```

Everything below is generated from a published build, so the table
definitions — including the comments explaining them — are the ones actually
in the file.

## `filmstock.db`

Every entity, every credit, and the search indexes.

As published (filmstock.db): 378 MB.

### credits — 1,416,234 rows

```sql
CREATE TABLE credits(person_id INTEGER, work_id INTEGER, work_type TEXT, role TEXT,
  PRIMARY KEY (person_id, work_id, work_type, role)) WITHOUT ROWID;
```

Indexes:

```sql
    CREATE INDEX idx_credits_work ON credits(work_id, work_type);
```

### events — 4,706 rows

```sql
CREATE TABLE events(
  id INTEGER PRIMARY KEY, title TEXT NOT NULL, kind TEXT NOT NULL,
  award TEXT, edition INTEGER, date TEXT, year INTEGER,
  hosts TEXT, organizer TEXT, venue TEXT, location TEXT, network TEXT,
  best_film TEXT, most_wins TEXT, opening_film TEXT, closing_film TEXT,
  cover_image_file TEXT, wikipedia_url TEXT
);
```

Indexes:

```sql
    CREATE INDEX idx_events_kind ON events(kind);
    CREATE INDEX idx_events_year ON events(year);
```

### external_ids — 655,765 rows

```sql
CREATE TABLE external_ids(
  id     INTEGER NOT NULL,   -- our identity: the enwiki page_id
  kind   TEXT    NOT NULL,   -- movies | television | people
  source TEXT    NOT NULL,   -- imdb | tmdb_movie | tmdb_tv | tvdb | rotten_tomatoes
  value  TEXT    NOT NULL,
  PRIMARY KEY (id, kind, source, value)
) WITHOUT ROWID;
```

Indexes:

```sql
    CREATE INDEX idx_external_source ON external_ids(source, value);
```

### franchise_members — 4,050 rows

```sql
CREATE TABLE franchise_members(
  franchise_id INTEGER NOT NULL, id INTEGER NOT NULL, kind TEXT NOT NULL,
  PRIMARY KEY (franchise_id, id, kind)
) WITHOUT ROWID;
```

Indexes:

```sql
    CREATE INDEX idx_franchise_members_id ON franchise_members(id);
```

### franchises — 702 rows

```sql
CREATE TABLE franchises(
  id INTEGER PRIMARY KEY, qid INTEGER NOT NULL, title TEXT NOT NULL
);
```

Indexes:

```sql
    CREATE INDEX idx_franchises_qid ON franchises(qid);
```

### movies — 166,159 rows

```sql
CREATE TABLE movies(
  id INTEGER PRIMARY KEY,
  title TEXT NOT NULL,
  year INTEGER,
  release_date TEXT,
  director TEXT, producer TEXT, writer TEXT, starring TEXT,
  music TEXT, distributor TEXT, country TEXT, language TEXT, genre TEXT,
  runtime TEXT, budget TEXT, gross TEXT,
  wikipedia_url TEXT, cover_image_url TEXT, cover_image_file TEXT,
  -- The article title, disambiguator and all.
  --
  -- title is the DISPLAY title with Wikipedia's parenthetical removed, because
  -- "(1985 film)" is namespacing rather than part of the name. But 11,925 titles
  -- are shared by 31,934 films: six Vertigos, five Heats, two Godfathers — the
  -- 175-minute cut and the 539-minute television version. Stripped, they are one
  -- string repeated, and the only thing that ever told them apart was the
  -- parenthetical.
  --
  -- Nothing keys on this. Identity is the page_id, as everywhere else.
  wiki_title TEXT
);
```

Indexes:

```sql
    CREATE INDEX idx_movies_title ON movies(title);
    CREATE INDEX idx_movies_wiki_title ON movies(wiki_title);
    CREATE INDEX idx_movies_year ON movies(year);
```

### people — 235,804 rows

```sql
CREATE TABLE people(id INTEGER PRIMARY KEY, page_id INTEGER, qid INTEGER,
  name TEXT NOT NULL, wiki TEXT, image_url TEXT);
```

Indexes:

```sql
    CREATE INDEX idx_people_name ON people(name);
    CREATE INDEX idx_people_qid ON people(qid);
```

### person_alias — 235,810 rows

```sql
CREATE TABLE person_alias(wiki TEXT PRIMARY KEY, person_id INTEGER) WITHOUT ROWID;
```

### schedule_slots — 48,534 rows

```sql
CREATE TABLE schedule_slots(
  -- Content-derived, not allocated: a stable hash of the row's identity, so
  -- identical content gets identical ids in every build and record-level
  -- diffs between builds carry changes, not renumbering.
  id INTEGER PRIMARY KEY,
  schedule_id INTEGER NOT NULL,
  day TEXT NOT NULL, network TEXT NOT NULL,
  start TEXT NOT NULL, end TEXT NOT NULL,
  part TEXT, title TEXT NOT NULL, show_id INTEGER,
  rerun INTEGER, rank INTEGER, rating REAL
);
```

Indexes:

```sql
    CREATE INDEX idx_slots_schedule ON schedule_slots(schedule_id);
    CREATE INDEX idx_slots_show ON schedule_slots(show_id);
    CREATE INDEX idx_slots_when ON schedule_slots(day, start);
```

### schedules — 232 rows

```sql
CREATE TABLE schedules(
  id INTEGER PRIMARY KEY, title TEXT NOT NULL,
  season TEXT, daypart TEXT, wikipedia_url TEXT
);
```

Indexes:

```sql
    CREATE INDEX idx_schedules_season ON schedules(season);
```

### sequels — 0 rows

```sql
CREATE TABLE sequels(
  id INTEGER NOT NULL, kind TEXT NOT NULL, next_id INTEGER NOT NULL,
  PRIMARY KEY (id, kind, next_id)
) WITHOUT ROWID;
```

Indexes:

```sql
    CREATE INDEX idx_sequels_next ON sequels(next_id);
```

### television_episodes — 669,380 rows

```sql
CREATE TABLE television_episodes(
  -- Content-derived, not allocated: a stable hash of the row's identity, so
  -- identical content gets identical ids in every build and record-level
  -- diffs between builds carry changes, not renumbering.
  id INTEGER PRIMARY KEY,
  series_id INTEGER, season INTEGER,
  number_in_season INTEGER, number_overall INTEGER, title TEXT, air_date TEXT,
  viewers REAL
);
```

Indexes:

```sql
    CREATE INDEX idx_television_ep_series ON television_episodes(series_id);
```

### television_seasons — 51,052 rows

```sql
CREATE TABLE television_seasons(
  -- Content-derived, not allocated: a stable hash of the row's identity, so
  -- identical content gets identical ids in every build and record-level
  -- diffs between builds carry changes, not renumbering.
  id INTEGER PRIMARY KEY,
  series_id INTEGER NOT NULL, season INTEGER NOT NULL, page_id INTEGER,
  num_episodes INTEGER, first_aired TEXT, last_aired TEXT,
  network TEXT, starring TEXT, image TEXT,
  rank INTEGER, rating REAL, viewers REAL
);
```

Indexes:

```sql
    CREATE INDEX idx_television_seasons_series ON television_seasons(series_id, season);
```

### television_series — 65,172 rows

```sql
CREATE TABLE television_series(
  id INTEGER PRIMARY KEY, title TEXT NOT NULL, year INTEGER,
  first_aired TEXT, last_aired TEXT, genre TEXT, creator TEXT, starring TEXT,
  network TEXT, num_seasons TEXT, num_episodes TEXT,
  seasons_count INTEGER, episodes_count INTEGER,
  cover_image_file TEXT, cover_image_url TEXT, wikipedia_url TEXT,
  -- As with films: 2,208 titles are shared by 5,215 series. Five different
  -- shows are called Friends, five are called The Office.
  wiki_title TEXT
);
```

Indexes:

```sql
    CREATE INDEX idx_television_title ON television_series(title);
    CREATE INDEX idx_television_wiki_title ON television_series(wiki_title);
```

### events_fts — ships empty, rebuild locally

```sql
CREATE VIRTUAL TABLE events_fts USING fts5(
  title, award, hosts,
  content='events', content_rowid='id', tokenize='trigram'
);
```

### movies_fts — ships empty, rebuild locally

```sql
CREATE VIRTUAL TABLE movies_fts USING fts5(
  title, starring, director,
  content='movies', content_rowid='id', tokenize='trigram'
);
```

### people_fts — ships empty, rebuild locally

```sql
CREATE VIRTUAL TABLE people_fts USING fts5(name, content='people', content_rowid='id', tokenize='trigram');
```

### television_episodes_fts — ships empty, rebuild locally

```sql
CREATE VIRTUAL TABLE television_episodes_fts USING fts5(
  title, content='television_episodes', content_rowid='id', tokenize='trigram'
);
```

### television_fts — ships empty, rebuild locally

```sql
CREATE VIRTUAL TABLE television_fts USING fts5(
  title, starring, creator,
  content='television_series', content_rowid='id', tokenize='trigram'
);
```

## `filmstock-text.db`

Prose: overviews, plots and episode summaries. Ships separately because a consumer that only searches never needs it.

As published (filmstock-text.db): 651 MB.

### episode_text — 444,183 rows

```sql
CREATE TABLE episode_text(
  id INTEGER PRIMARY KEY, series_id INTEGER, summary TEXT
);
```

Indexes:

```sql
    CREATE INDEX idx_episode_text_series ON episode_text(series_id);
```

### movie_text — 166,156 rows

```sql
CREATE TABLE movie_text(
  id INTEGER PRIMARY KEY, overview TEXT, plot TEXT
);
```

### television_text — 65,171 rows

```sql
CREATE TABLE television_text(
  id INTEGER PRIMARY KEY, overview TEXT, plot TEXT
);
```

## `filmstock-vectors.db`

Embedding vectors, for similarity and recommendation.

As published (filmstock-vectors.db): 222 MB.

### vector_meta — 1 rows

```sql
CREATE TABLE vector_meta(
  model TEXT NOT NULL,
  dims  INTEGER NOT NULL,
  count INTEGER NOT NULL,
  lo    BLOB NOT NULL,
  span  BLOB NOT NULL
);
```

### vectors — 170,421 rows

```sql
CREATE TABLE vectors(
  id INTEGER PRIMARY KEY,   -- the work's page_id
  v  BLOB NOT NULL          -- dims × int8, quantised
);
```
