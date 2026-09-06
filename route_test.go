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
