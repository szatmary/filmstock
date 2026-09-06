package build

// The synopses, in the core database.
//
// They used to be a second file, and the reasoning was sound while it held:
// the synopses were 462 MB of a 974 MB total, and a media application needs
// titles, credits, air dates and schedules to answer "what did this person
// appear in" without paying for every plot.
//
// Almost all of that weight was `plot`. Dropping it leaves the article lead as
// an overview and a line per episode — the text a UI actually renders — and
// that is small enough that a second file costs a consumer more in
// coordination than it saves in bytes. Two files that must be fetched,
// verified and kept at the same build to be joinable is a real burden; the
// remainder does not justify it.
//
// The plot text is still extracted into the intermediate, so this is a
// publishing decision and not a lossy one: an export can put it back without
// re-reading a dump. It is also load-bearing at build time, where the passage
// centroid that recovers Philip K. Dick for Blade Runner is built from it.
const textSchema = `
CREATE TABLE IF NOT EXISTS movie_text(
  id INTEGER PRIMARY KEY, overview TEXT
);
CREATE TABLE IF NOT EXISTS television_text(
  id INTEGER PRIMARY KEY, overview TEXT
);
-- Keyed by the episode's rowid in television_episodes, which is an
-- AUTOINCREMENT id rather than anything the encyclopaedia states.
CREATE TABLE IF NOT EXISTS episode_text(
  id INTEGER PRIMARY KEY, series_id INTEGER, summary TEXT
);
CREATE INDEX IF NOT EXISTS idx_episode_text_series ON episode_text(series_id);
`
