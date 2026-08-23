package build

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/szatmary/filmstock"
	"github.com/szatmary/filmstock/internal/dump"
)

// televisionUpdater merges a day's television changes into series records that
// already exist in the store.
//
// A full pass can assemble a series from scratch because it sees every article
// that describes it. An incremental pass sees only what changed, and a season
// article names its season but never its series — so the only way to attach it
// is to load the series record the store already holds and replace the part the
// changed article describes. Everything the day did not mention is left alone,
// which is both the correct answer and the reason the diff stays small.
type televisionUpdater struct {
	w        *storeWriter
	seasonOf map[int]int // season/list article page_id -> series page_id, from Wikidata

	// Changes are collected during the stream and applied at the end: a single
	// series can be touched by several articles in one day, and rewriting its
	// record once per article would churn the store for no reason.
	pending map[int]*pendingSeries

	stats tvUpdateStats
}

type pendingSeries struct {
	meta    *filmstock.TelevisionSeries // set when the series' OWN article changed
	seasons map[int]*filmstock.Season   // season number -> replacement
}

type tvUpdateStats struct {
	seriesMeta  int // series articles whose own metadata changed
	seasonsSet  int // seasons replaced from a changed source article
	seriesWrote int // series records actually rewritten
	unresolved  int // sources whose owning series could not be established
	notInStore  int // owners resolved, but no such series record
	unchangedTV int
}

func newTelevisionUpdater(w *storeWriter, cachePath string) (*televisionUpdater, error) {
	seasonOf, err := loadSeasonOf(cachePath)
	if err != nil {
		return nil, fmt.Errorf("television updates need the Wikidata resolver cache: %w", err)
	}
	return &televisionUpdater{
		w:        w,
		seasonOf: seasonOf,
		pending:  map[int]*pendingSeries{},
	}, nil
}

func (u *televisionUpdater) get(seriesID int) *pendingSeries {
	p, ok := u.pending[seriesID]
	if !ok {
		p = &pendingSeries{seasons: map[int]*filmstock.Season{}}
		u.pending[seriesID] = p
	}
	return p
}

// consider routes one changed page's television messages to the series they
// belong to.
func (u *televisionUpdater) consider(p dump.Page) {
	for _, m := range parseTelevisionPage(p) {
		switch {
		case m.series != nil:
			// The series' own article changed: its metadata is authoritative,
			// but its seasons are not — most of them come from other articles.
			u.get(m.series.PageID).meta = m.series
			u.stats.seriesMeta++

		case m.srcID != 0 && (len(m.eps) > 0 || m.seasonMeta != nil):
			owner := m.seriesID
			if owner == 0 {
				// A season article states its season, never its series. Only
				// Wikidata's stated P179/P4908 edge can attach it; guessing from
				// the title is what merges fifty different "Big Brother"s.
				owner = u.seasonOf[m.srcID]
			}
			if owner == 0 {
				u.stats.unresolved++
				continue
			}
			s := m.seasonMeta
			if s == nil {
				s = &filmstock.Season{Season: m.season}
			}
			s.Episodes = m.eps
			s.NumEpisodes = len(m.eps)
			u.get(owner).seasons[m.season] = s
			u.stats.seasonsSet++
		}
	}
}

// apply merges everything collected into the stored records.
func (u *televisionUpdater) apply(dry bool) {
	ids := make([]int, 0, len(u.pending))
	for id := range u.pending {
		ids = append(ids, id)
	}
	sort.Ints(ids) // deterministic write order

	for _, id := range ids {
		pend := u.pending[id]
		cur := u.w.get(filmstock.KindTelevision, int64(id))
		if cur == nil {
			// The day changed a season of a series this store has never held.
			// Creating one from a season article alone would be a series record
			// with no series article behind it.
			u.stats.notInStore++
			continue
		}
		var rec filmstock.TelevisionSeries
		if json.Unmarshal(cur, &rec) != nil {
			continue
		}

		if pend.meta != nil {
			// Keep the seasons already assembled; take everything else from the
			// article that just changed.
			seasons := rec.Seasons
			rec = *pend.meta
			rec.Seasons = seasons
		}
		for num, s := range pend.seasons {
			replaceSeason(&rec, num, s)
		}
		sort.SliceStable(rec.Seasons, func(i, j int) bool {
			return rec.Seasons[i].Season < rec.Seasons[j].Season
		})
		if !dry {
			u.w.put(filmstock.KindTelevision, int64(id), &rec)
		}
		u.stats.seriesWrote++
	}
}

// replaceSeason swaps one season in place, or appends it when the series did not
// have it. Other seasons are untouched: the day said nothing about them.
func replaceSeason(rec *filmstock.TelevisionSeries, num int, s *filmstock.Season) {
	for i, existing := range rec.Seasons {
		if existing.Season == num {
			rec.Seasons[i] = s
			return
		}
	}
	rec.Seasons = append(rec.Seasons, s)
}

func (u *televisionUpdater) report() {
	fmt.Fprintf(os.Stderr, "  television   %d series merged (%d metadata, %d seasons replaced)\n",
		u.stats.seriesWrote, u.stats.seriesMeta, u.stats.seasonsSet)
	if u.stats.unresolved > 0 || u.stats.notInStore > 0 {
		fmt.Fprintf(os.Stderr, "               %d sources with no stated series, %d owners not in the store\n",
			u.stats.unresolved, u.stats.notInStore)
	}
}
