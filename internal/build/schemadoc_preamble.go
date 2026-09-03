package build

// schemaPreamble is the part a generator cannot derive: what the files are,
// how to open them, and the conventions a reader has to know before the table
// definitions mean anything.
const schemaPreamble = "# The filmstock schema\n" + `
Three SQLite files. Open them with any SQLite library, in any language — the
Go package here is for fetching and updating them, not for querying them.
Nothing below needs it.

## Reading them together

The files are separate so a consumer downloads only what it uses, but they
join on shared ids. Attach them and query across:

` + "```sql" + `
ATTACH DATABASE 'filmstock-text.db'    AS text;
ATTACH DATABASE 'filmstock-vectors.db' AS vec;

SELECT m.title, m.year, t.plot
FROM movies m
JOIN text.movie_text t ON t.id = m.id
WHERE m.year = 1954;
` + "```" + `

## Identity

**Every id is the English Wikipedia page_id of the thing's own article.** Not a
row number, not a hash, not a title. It is stable across builds, so a consumer
may store it, and it is the join key everywhere: ` + "`movies.id`" + `,
` + "`television_series.id`" + `, ` + "`events.id`" + `, ` + "`text.movie_text.id`" + `,
` + "`vec.vectors.id`" + ` all mean the same article.

Two exceptions, both deliberate:

- **People with no article.** A credit naming someone who has no Wikipedia
  page still counts, so they get a row whose id is derived from their link
  target with bit 31 set, keeping it clear of real page_ids. Their
  ` + "`page_id`" + ` column is 0.
- **Rows for things that are not articles** — seasons, episodes, schedule
  slots — get an id derived from their content (series, season number, title
  and so on). It is stable between builds for the same content, which is what
  lets daily updates ship as small diffs, but it re-keys if the content it is
  derived from changes.

## Conventions

- **List-shaped columns are display strings**, joined with " · " (space, middle
  dot, space): ` + "`starring`" + `, ` + "`genre`" + `, ` + "`country`" + `,
  ` + "`language`" + `. Split on that separator if you need the elements. They
  are for showing and for LIKE; for anything relational, join ` + "`credits`" + `.
- **Titles are stripped of Wikipedia's disambiguator** — "Batman (1989 film)"
  is stored as ` + "`title` = 'Batman'" + `, with the original in
  ` + "`wiki_title`" + `. Since titles are not unique (11,925 titles are shared
  by 31,934 films), display the year alongside, and never key on a title.
- **Absent means absent.** An empty string or NULL means Wikipedia did not
  state it, never that the value is zero or unknown-but-guessed.
- **Dates are strings**, as stated: usually ` + "`YYYY-MM-DD`" + `, sometimes
  less precise. They sort correctly as text when fully specified.

## The full-text indexes ship EMPTY

` + "`movies_fts`" + ` and the other ` + "`*_fts`" + ` tables are declared but
carry no rows. They are derived data — 181 MB of it — that rebuilds locally in
seconds, and any daily update would stale them anyway. Rebuild them once after
downloading, then again after each update:

` + "```sql" + `
INSERT INTO movies_fts(movies_fts)              VALUES('rebuild');
INSERT INTO television_fts(television_fts)      VALUES('rebuild');
INSERT INTO television_episodes_fts(television_episodes_fts) VALUES('rebuild');
INSERT INTO events_fts(events_fts)              VALUES('rebuild');
INSERT INTO people_fts(people_fts)              VALUES('rebuild');
` + "```" + `

They are FTS5 external-content tables over the base tables, tokenized
` + "`trigram`" + `, so they match substrings and tolerate typos when combined
with your own ranking. The Go updater does this rebuild for you.

## Credits

` + "`credits`" + ` is the relational join between people and works, and the one
place to ask "what did this person do" or "who worked on this":

` + "```sql" + `
SELECT p.name, c.role
FROM credits c JOIN people p ON p.id = c.person_id
WHERE c.work_id = 31371 AND c.work_type = 'movie';
` + "```" + `

` + "`work_type`" + ` is ` + "`movie`" + `, ` + "`television`" + ` or
` + "`event`" + `, and ` + "`work_id`" + ` is that work's id. A person credited
under an old article title still resolves, because ` + "`person_alias`" + ` maps
every link target to the person it redirects to.

## External identifiers

` + "`external_ids`" + ` carries IMDb, TMDb, TVDB and Rotten Tomatoes ids from
Wikidata — join keys only, no ratings or content from those services:

` + "```sql" + `
SELECT value FROM external_ids
WHERE id = 31371 AND kind = 'movies' AND source = 'imdb';
` + "```" + `

## Regenerating this document

` + "```" + `
filmstock schema -db bucket/<build>/filmstock.db -out docs/schema.md
` + "```" + `

Everything below is generated from a published build, so the table
definitions — including the comments explaining them — are the ones actually
in the file.
`
