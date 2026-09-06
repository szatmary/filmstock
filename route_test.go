package filmstock

import "testing"

// A catalog shaped like the real one: a full, then dailies, with rollup edges
// spanning 7 back on every build that has one.
func chainOf(days int, fullBytes, daily, rollup int64) *catalog {
	c := &catalog{LatestFull: "F", Latest: "F"}
	c.Builds = append(c.Builds, catalogEntry{
		ID: "F", Kind: "full", Through: "20260801", Bytes: fullBytes})
	prev := "F"
	for i := 1; i <= days; i++ {
		id := day(i)
		e := catalogEntry{ID: id, Kind: "daily", Through: id, Parent: prev,
			Edges: []patchEdge{{From: prev, Bytes: daily}}}
		if i > 7 {
			from := day(i - 7)
			e.Edges = append(e.Edges, patchEdge{
				From: from, Suffix: ".from-" + from, Bytes: rollup})
		} else if i == 7 {
			e.Edges = append(e.Edges, patchEdge{From: "F", Suffix: ".from-F", Bytes: rollup})
		}
		c.Builds = append(c.Builds, e)
		c.Latest = id
		prev = id
	}
	return c
}

func day(i int) string {
	return string(rune('0'+i/100%10)) + string(rune('0'+i/10%10)) + string(rune('0'+i%10))
}

// The four cases the byte rule has to get right without being told about any
// of them. This is the whole argument for choosing by cost rather than
// prescribing a route in the catalog.
func TestCheapestRouteSuitsEveryConsumer(t *testing.T) {
	// A daily patch is small, a week's rollup is bigger than one daily but far
	// smaller than seven, and the full dwarfs both.
	c := chainOf(60, 1_300_000_000, 1_000_000, 4_000_000)
	latest := c.Latest

	t.Run("daily follower takes one patch", func(t *testing.T) {
		full, steps, cost := c.cheapest(day(59), latest)
		if full != "" || len(steps) != 1 {
			t.Fatalf("full=%q steps=%d cost=%d; want one patch", full, len(steps), cost)
		}
		if steps[0].suffix != "" {
			t.Errorf("took a rollup for a single day: %q", steps[0].suffix)
		}
	})

	t.Run("a month behind takes rollups, not thirty dailies", func(t *testing.T) {
		full, steps, cost := c.cheapest(day(28), latest)
		if full != "" {
			t.Fatalf("refetched a full when patches were cheaper: %s", full)
		}
		if len(steps) >= 30 {
			t.Fatalf("walked %d steps; the rollups exist to avoid that", len(steps))
		}
		// 32 days at 1 MB each vs ~4 rollups + a few dailies.
		if cost >= 32*1_000_000 {
			t.Errorf("cost %d is no better than walking every daily", cost)
		}
		rollups := 0
		for _, s := range steps {
			if s.suffix != "" {
				rollups++
			}
		}
		if rollups == 0 {
			t.Error("no rollup used on a month-long catch-up")
		}
	})

	t.Run("fresh install takes the full", func(t *testing.T) {
		full, _, _ := c.cheapest("", latest)
		if full != "F" {
			t.Fatalf("fresh install did not seed from the full: %q", full)
		}
	})

	t.Run("held build the catalog forgot still moves", func(t *testing.T) {
		// Not a downgrade and not a refusal: seed from the full and ride on.
		full, _, _ := c.cheapest("some-pruned-build", latest)
		if full != "F" {
			t.Fatalf("a consumer holding an unlisted build was stranded: %q", full)
		}
	})
}

// When the patch road costs more than simply downloading the newest full, the
// search says so on its own. Nobody configures a cutoff.
//
// The full that matters is the newest one, near the tip — the monthly rebuild —
// not the one the chain started from. Downloading a full at the far end of the
// chain leaves every intervening day still to walk, which is no shortcut at all.
func TestCheapestPrefersTheFullWhenPatchesCostMore(t *testing.T) {
	c := chainOf(30, 9_000_000_000, 900_000, 900_000)
	// A fresh monthly full lands at the tip, bridging from the last daily.
	c.Builds = append(c.Builds, catalogEntry{
		ID: "G", Kind: "full", Through: "999", BridgeFrom: c.Latest, Bytes: 1_000_000,
		Edges: []patchEdge{{From: c.Latest, Bytes: 200_000}},
	})
	c.Latest, c.LatestFull = "G", "G"

	// From 25 days back the patch road is several 900 KB hops plus the bridge,
	// well past the 1 MB the full costs.
	full, steps, cost := c.cheapest(day(5), "G")
	if full != "G" {
		t.Fatalf("kept patching (%d steps, %d bytes) when the 1 MB full was cheaper",
			len(steps), cost)
	}
	if len(steps) != 0 {
		t.Errorf("downloaded the full and then still applied %d patches", len(steps))
	}

	// And the follower one hop away takes the 200 KB bridge rather than
	// refetching 1 MB: the same rule, the opposite answer.
	full, steps, _ = c.cheapest(c.Builds[len(c.Builds)-2].ID, "G")
	if full != "" || len(steps) != 1 {
		t.Fatalf("one step from the tip: full=%q steps=%d; want a single bridge", full, len(steps))
	}
}

// A catalog published before edges existed still resolves: each build's single
// parent/bridge line is the one route in.
func TestCheapestFallsBackToTheParentLine(t *testing.T) {
	c := &catalog{LatestFull: "F", Latest: "002", Builds: []catalogEntry{
		{ID: "F", Kind: "full", Through: "20260801", Bytes: 500},
		{ID: "001", Kind: "daily", Through: "20260802", Parent: "F"},
		{ID: "002", Kind: "daily", Through: "20260803", Parent: "001"},
	}}
	full, steps, _ := c.cheapest("F", "002")
	if full != "" || len(steps) != 2 {
		t.Fatalf("full=%q steps=%d; want the two-step parent line", full, len(steps))
	}
	if steps[0].entry.ID != "001" || steps[1].entry.ID != "002" {
		t.Fatalf("steps out of order: %s then %s", steps[0].entry.ID, steps[1].entry.ID)
	}
}

func TestCheapestReportsWhenNothingReaches(t *testing.T) {
	c := &catalog{Latest: "z", Builds: []catalogEntry{
		{ID: "a", Kind: "daily", Through: "20260801"},
		{ID: "z", Kind: "daily", Through: "20260901"}, // no parent, no edges
	}}
	full, steps, _ := c.cheapest("a", "z")
	if full != "" || steps != nil {
		t.Fatalf("invented a route: full=%q steps=%v", full, steps)
	}
}

// Starting fresh. An epoch is bumped to declare that nothing held from before
// can be carried forward — a schema change old patches cannot express, or a
// chain found to be wrong. The consumer must take the full road on purpose,
// not by discovering that a patch will not verify.
func TestFreshEpochStrandsNoOneAndCarriesNothingForward(t *testing.T) {
	c := chainOf(20, 1_300_000_000, 1_000_000, 4_000_000)
	oldTip := c.Latest
	// The new lineage: a full with no bridge and no edges at all.
	c.Builds = append(c.Builds, catalogEntry{
		ID: "N", Kind: "full", Through: "999", Bytes: 900_000_000,
		Epoch: 1, EpochReason: "episodes gained a column patches cannot express",
	})
	c.Latest, c.LatestFull = "N", "N"

	// A consumer on the old lineage cannot patch across, however little they
	// are behind: the only way in is the new full, whole.
	full, steps, _ := c.cheapest(oldTip, "N")
	if full != "N" {
		t.Fatalf("crossed a lineage break: full=%q steps=%d", full, len(steps))
	}
	if len(steps) != 0 {
		t.Errorf("applied %d patch(es) across an epoch boundary", len(steps))
	}

	// Nor may a build in the new epoch be used as a stepping stone into the
	// old one. Asking for a route to a retired build still answers — cheapest
	// is a path finder, and refusing a downgrade is update's job — but the
	// answer must re-seed from a full of that lineage rather than continue
	// from what is held.
	if seed, _, _ := c.cheapest("N", oldTip); seed != "F" {
		t.Errorf("continued out of the new epoch into the old instead of re-seeding: %q", seed)
	}
}

// Within one epoch nothing changes: the break is a declaration, not a new
// default.
func TestSameEpochStillPatchesNormally(t *testing.T) {
	c := chainOf(20, 1_300_000_000, 1_000_000, 4_000_000)
	full, steps, _ := c.cheapest(day(19), c.Latest)
	if full != "" || len(steps) != 1 {
		t.Fatalf("full=%q steps=%d; want the ordinary single patch", full, len(steps))
	}
}

// The years-behind case, and where it stops being worth walking.
//
// Positional rollups can have their source pruned, so a consumer years behind
// cannot rely on them. The full-to-full edge can always be built — a full's
// databases are hosted permanently — so the hop count is one per month and does
// not depend on which spans survived.
//
// Bounded hops is not the same as a cheaper path, and the byte rule is what
// tells them apart: far enough back, re-downloading beats walking, and the
// search says so without being told where the line is.
func monthlyFulls(months int, hop, fullBytes int64) *catalog {
	c := &catalog{}
	prev := ""
	for m := 0; m < months; m++ {
		id := "f" + day(m)
		e := catalogEntry{ID: id, Kind: "full", Through: day(m), Bytes: fullBytes}
		if prev != "" {
			// Only the month-to-month edge: every daily and every positional
			// rollup between them has long since been pruned.
			e.Edges = []patchEdge{{From: prev, Suffix: ".from-" + prev, Bytes: hop}}
			e.BridgeFrom = prev
		}
		c.Builds = append(c.Builds, e)
		c.Latest, c.LatestFull = id, id
		prev = id
	}
	return c
}

func TestFullToFullWalksWhileItIsWorthIt(t *testing.T) {
	// Six months at 40 MB a hop is 200 MB against a 1.3 GB full: walk.
	c := monthlyFulls(6, 40_000_000, 1_300_000_000)
	seed, steps, cost := c.cheapest("f"+day(0), c.Latest)
	if seed != "" {
		t.Fatalf("refetched a full when 5 month-hops cost far less")
	}
	if len(steps) != 5 {
		t.Fatalf("steps=%d; want one per month", len(steps))
	}
	if cost != 5*40_000_000 {
		t.Errorf("cost=%d; want the five hops", cost)
	}
}

func TestFullToFullStopsBeingWorthItEventually(t *testing.T) {
	// Three years at 40 MB a hop is 1.4 GB, past the 1.3 GB full. Bounded hops
	// were never the goal in themselves — fewest bytes was.
	c := monthlyFulls(36, 40_000_000, 1_300_000_000)
	seed, steps, cost := c.cheapest("f"+day(0), c.Latest)
	if seed != c.Latest {
		t.Fatalf("walked %d hops for %d bytes when the 1.3 GB full was cheaper",
			len(steps), cost)
	}
	if cost != 1_300_000_000 {
		t.Errorf("cost=%d; want the full's price", cost)
	}
}
