package build

import (
	"database/sql"
	"os"
	"testing"

	"github.com/szatmary/filmstock/internal/record"
	"github.com/szatmary/filmstock/internal/sqldrv"
	"github.com/szatmary/filmstock/internal/wikitext"
)

// How close can we get to a TV Guide page for one night?
//
// The schedule says what occupied each slot across a season. The episode
// records say which episode aired on a given date. Joined on the show, they
// give what a listing needs: time, network, programme, and which episode.
func TestReconstructOneNight(t *testing.T) {
	const date, day = "1996-11-14", "Thursday" // a Thursday in November 1996

	s := buildSchedule(*loadSchedule(t, "1996-97"))
	if s == nil {
		t.Skip("no schedule")
	}
	cache := os.Getenv("HOME") + "/.cache/filmstock/resolver.db"
	if _, err := os.Stat(cache); err != nil {
		t.Skip("no resolver cache")
	}
	rdb, err := sql.Open(sqldrv.Name, cache)
	if err != nil {
		t.Skip(err)
	}
	defer rdb.Close()
	look, err := rdb.Prepare(`SELECT page_id FROM wiki_qid WHERE title = ?`)
	if err != nil {
		t.Skip(err)
	}
	defer look.Close()
	ResolveShows(s, func(title string) int {
		var id int
		look.QueryRow(title).Scan(&id)
		return id
	})

	idx, err := sql.Open(sqldrv.Name, "/var/tmp/gapcheck.db")
	if err != nil {
		t.Skip(err)
	}
	defer idx.Close()
	ep, err := idx.Prepare(`SELECT e.title FROM television_episodes e
	                        WHERE e.series_id = ? AND e.air_date = ?`)
	if err != nil {
		t.Skip(err)
	}
	defer ep.Close()

	var slots, linked, withEp int
	t.Logf("  ==== %s, %s ====", date, day)
	last := ""
	for _, e := range s.Entries {
		if e.Day != day || (e.Part != "Fall" && e.Part != "") {
			continue
		}
		slots++
		if e.ShowID != 0 {
			linked++
		}
		var epTitle string
		if e.ShowID != 0 {
			ep.QueryRow(e.ShowID, date).Scan(&epTitle)
			if epTitle != "" {
				withEp++
			}
		}
		if e.Network != last {
			t.Logf("  %s", e.Network)
			last = e.Network
		}
		line := "    " + e.Start + "  " + e.Title
		if epTitle != "" {
			line += "  — \"" + epTitle + "\""
		} else if e.ShowID == 0 {
			line += "   [not linked]"
		}
		t.Logf("%s", line)
	}
	t.Logf("\n  %d slots, %d linked to a series (%d%%), %d resolved to an episode (%d%%)",
		slots, linked, 100*linked/max(slots, 1), withEp, 100*withEp/max(slots, 1))
	_ = wikitext.CanonTitle
	if linked == 0 {
		t.Error("nothing linked — the join to series records is broken")
	}
	_ = record.Schedule{}
}
