package query

import (
	"math"
	"math/rand"
	"testing"
)

// A corpus with a known shape: two planted directions plus noise. If the local
// axes are real, exploration should find them — and moving along one should
// change that coordinate and leave the other alone.
func plantedCorpus(t *testing.T, n, dims int) (*Vectors, []int) {
	t.Helper()
	rng := rand.New(rand.NewSource(1))
	ids := make([]int32, n)
	rows := make([][]float32, n)
	for i := range n {
		ids[i] = int32(i + 1)
		r := make([]float32, dims)
		for d := range dims {
			r[d] = float32(rng.NormFloat64()) * 0.02
		}
		r[0] += float32(rng.NormFloat64())      // planted axis A, widest
		r[1] += float32(rng.NormFloat64()) * .5 // planted axis B
		r[2] += 3                               // shared offset, so all are neighbours
		rows[i] = r
	}
	v, err := OpenVectors(writeVectors(t, ids, rows))
	if err != nil {
		t.Fatal(err)
	}
	all := make([]int, n)
	for i := range n {
		all[i] = i + 1
	}
	return v, all
}

func TestLocalAxesFindPlantedStructure(t *testing.T) {
	v, all := plantedCorpus(t, 400, 24)
	c, _ := v.Collect(all)
	e, err := c.Explore(all[0])
	if err != nil {
		t.Fatal(err)
	}
	view, err := e.View(0)
	if err != nil {
		t.Fatal(err)
	}
	// The widest planted direction is dimension 0; the first axis should be
	// mostly aligned with it.
	a1 := view.axes[0]
	if math.Abs(float64(a1[0])) < 0.7 {
		t.Errorf("axis 1 = %v; expected it to align with the planted dimension 0", a1[:3])
	}
	if view.AxisVar[0] <= view.AxisVar[1] {
		t.Errorf("axes out of order: %.3f then %.3f", view.AxisVar[0], view.AxisVar[1])
	}
	// The two axes must be independent, or the second is just the first again.
	if p := math.Abs(float64(dot(view.axes[0], view.axes[1]))); p > 1e-3 {
		t.Errorf("axes are not orthogonal: dot = %.5f", p)
	}
}

// A press must move the position. This failed for real: pulling toward the
// centroid of the chosen side barely moved anything, because in a dense
// neighbourhood that centroid sits almost on top of where you already are.
func TestMoveActuallyMovesThePosition(t *testing.T) {
	v, all := plantedCorpus(t, 400, 24)
	c, _ := v.Collect(all)
	e, _ := c.Explore(all[0])
	before := append([]float32(nil), e.pos...)
	if _, err := e.Move(Right, 10); err != nil {
		t.Fatal(err)
	}
	if sim := dot(before, e.pos); sim > 0.999 {
		t.Fatalf("position barely moved: cosine to the starting point is %.5f", sim)
	}
}

// Opposite presses must go opposite ways.
//
// They do NOT exactly cancel, and asserting that they do was wrong: the axes are
// recomputed at the new position, so the direction "left" means after a step is
// not the reverse of the direction "right" meant before it. Path dependence is
// inherent to walking a manifold. What must hold is that going back is nearer
// the start than going on — otherwise the control has no reverse at all.
//
// Sign alignment across steps is what makes even this true. Without it a
// principal component flips freely between steps, and right-then-left took the
// position FURTHER from the start (cosine 0.78 -> 0.20) rather than nearer.
func TestOppositePressGoesBackNotOnward(t *testing.T) {
	v, all := plantedCorpus(t, 400, 24)
	c, _ := v.Collect(all)

	back, _ := c.Explore(all[0])
	start := append([]float32(nil), back.pos...)
	back.Move(Right, 5)
	back.Move(Left, 5)

	onward, _ := c.Explore(all[0])
	onward.Move(Right, 5)
	onward.Move(Right, 5)

	if dot(start, back.pos) <= dot(start, onward.pos) {
		t.Errorf("left did not reverse: right,left is %.4f from the start "+
			"but right,right is %.4f — going back should be nearer",
			dot(start, back.pos), dot(start, onward.pos))
	}
}

func TestTrailRecordsEveryCentre(t *testing.T) {
	v, all := plantedCorpus(t, 300, 24)
	c, _ := v.Collect(all)
	e, _ := c.Explore(all[0])
	for range 4 {
		if _, err := e.Move(Up, 5); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(e.Trail()); got != 5 {
		t.Errorf("trail has %d entries, want 5 (start + 4 moves)", got)
	}
	if e.Trail()[0] != all[0] {
		t.Errorf("trail does not begin where the walk began")
	}
}

// Drift is the whole point of the idea: the direction travelled is a trait
// nobody named, and it has to be recoverable at the end.
func TestDriftPointsAlongTheDirectionTravelled(t *testing.T) {
	v, all := plantedCorpus(t, 400, 24)
	c, _ := v.Collect(all)
	e, _ := c.Explore(all[0])
	for range 5 {
		e.Move(Right, 5)
	}
	toward, away := e.Drift(5)
	if len(toward) != 5 || len(away) != 5 {
		t.Fatalf("Drift returned %d/%d, want 5/5", len(toward), len(away))
	}
	if toward[0].Score <= away[0].Score {
		t.Errorf("toward (%.3f) should outrank away (%.3f)", toward[0].Score, away[0].Score)
	}
}

// A walk that never moved has no direction, and must say so rather than return
// an arbitrary one.
func TestDriftIsEmptyBeforeAnyMove(t *testing.T) {
	v, all := plantedCorpus(t, 100, 24)
	c, _ := v.Collect(all)
	e, _ := c.Explore(all[0])
	if toward, away := e.Drift(3); toward != nil || away != nil {
		t.Error("a walk with no moves reported a drift direction")
	}
}

// A collection smaller than the requested neighbourhood must still work.
func TestTinyCollection(t *testing.T) {
	v, all := plantedCorpus(t, 12, 24)
	c, _ := v.Collect(all)
	e, err := c.Explore(all[0])
	if err != nil {
		t.Fatal(err)
	}
	view, err := e.Move(Right, 5)
	if err != nil {
		t.Fatalf("a 12-film collection should still explore: %v", err)
	}
	if len(view.Cells) == 0 {
		t.Error("no cells returned")
	}
}

// A UI wants one record per grid cell. Cells carry a scatter position, so
// something has to resolve collisions and empties — that is Grid's whole job.
func TestGridPlacesEachRecordOnce(t *testing.T) {
	v, all := plantedCorpus(t, 300, 24)
	c, _ := v.Collect(all)
	e, _ := c.Explore(all[0])
	view, err := e.View(24)
	if err != nil {
		t.Fatal(err)
	}
	g := view.Grid(5, 5)
	if len(g) != 5 || len(g[0]) != 5 {
		t.Fatalf("grid is %dx%d, want 5x5", len(g), len(g[0]))
	}
	seen := map[int]bool{}
	filled := 0
	for _, row := range g {
		for _, cell := range row {
			if cell == nil {
				continue
			}
			filled++
			if seen[cell.PageID] {
				t.Errorf("record %d placed in more than one cell", cell.PageID)
			}
			seen[cell.PageID] = true
		}
	}
	if filled != 24 {
		t.Errorf("filled %d cells from 24 records, want 24", filled)
	}
}

// Fewer records than cells must leave holes, not repeat or rearrange: a stable
// layout is what lets the eye follow a title across a press.
func TestGridLeavesHolesWhenShort(t *testing.T) {
	v, all := plantedCorpus(t, 300, 24)
	c, _ := v.Collect(all)
	e, _ := c.Explore(all[0])
	view, _ := e.View(6)
	g := view.Grid(4, 4)
	filled := 0
	for _, row := range g {
		for _, cell := range row {
			if cell != nil {
				filled++
			}
		}
	}
	if filled != 6 {
		t.Errorf("filled %d of 16 cells from 6 records, want 6", filled)
	}
}

// The middle of the screen should hold the best match, not whatever the
// iteration order reached first.
func TestGridPutsTheNearestInTheMiddle(t *testing.T) {
	v, all := plantedCorpus(t, 300, 24)
	c, _ := v.Collect(all)
	e, _ := c.Explore(all[0])
	view, _ := e.View(25)
	g := view.Grid(5, 5)
	mid := g[2][2]
	if mid == nil {
		t.Fatal("centre cell is empty")
	}
	// The centre cell's record should be among the closest to the centre of the
	// axes, not an outlier flung to a corner.
	var better int
	for _, cell := range view.Cells {
		if cell.X*cell.X+cell.Y*cell.Y < mid.X*mid.X+mid.Y*mid.Y {
			better++
		}
	}
	if better > 3 {
		t.Errorf("%d records sit nearer the origin than the one placed centre", better)
	}
}

func TestGridHandlesDegenerateSizes(t *testing.T) {
	v, all := plantedCorpus(t, 100, 24)
	c, _ := v.Collect(all)
	e, _ := c.Explore(all[0])
	view, _ := e.View(10)
	for _, d := range [][2]int{{0, 5}, {5, 0}, {1, 1}, {1, 7}} {
		g := view.Grid(d[0], d[1])
		if len(g) != max(d[1], 0) {
			t.Errorf("Grid(%d,%d) returned %d rows", d[0], d[1], len(g))
		}
	}
}
