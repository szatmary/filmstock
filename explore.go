package filmstock

import (
	"fmt"
	"math"
	"sort"
)

// Navigating the embedding space through a 2D control surface.
//
// The space has 1024 dimensions and no names for any of them. What it does have
// is local structure: within any neighbourhood, the films differ along a handful
// of directions far more than the rest. Those directions are recoverable — they
// are the principal components of the neighbourhood — and they turn out to be
// legible. Measured on real neighbourhoods they separate things like light from
// serious, modern blockbuster from classic, creature feature from gothic.
// Nobody labelled them; that is simply where the variance is.
//
// So the two screen axes are not fixed. They are recomputed at every step from
// whatever is nearby, which is what makes moving feel like exploring rather than
// scrolling: near one film the horizontal axis might separate eras, and two
// moves later it separates tone.
//
// The top two components explain only 10–15% of local variance, because a
// neighbourhood of similar films is genuinely high-dimensional. That is fine for
// a walk — each step uses the locally most informative plane — and would not be
// fine for a single fixed projection of the whole space.

// A Collection restricts exploration to the records a user actually has.
//
// This is what makes the idea work in practice. Over the full corpus the local
// axes come out as language and industry, because most of 165,000 films are
// regional cinema, and every random starting point is something nobody
// recognises. A library is self-filtering: everything in it was chosen.
type Collection struct {
	v    *Vectors
	rows []int32
}

// Collect restricts v to the given records. Records without a vector are
// skipped, and the count of those is returned so a caller can report coverage
// rather than silently exploring a subset.
func (v *Vectors) Collect(pageIDs []int) (*Collection, int) {
	c := &Collection{v: v}
	var missing int
	seen := make(map[int32]bool, len(pageIDs))
	for _, id := range pageIDs {
		i, ok := v.byID[int32(id)]
		if !ok {
			missing++
			continue
		}
		if !seen[i] {
			seen[i] = true
			c.rows = append(c.rows, i)
		}
	}
	return c, missing
}

// Len reports how many records the collection can explore.
func (c *Collection) Len() int { return len(c.rows) }

// Similar returns the n records in the collection closest to pageID.
func (c *Collection) Similar(pageID, n int) ([]Neighbour, error) {
	q, ok := c.v.Vector(pageID)
	if !ok {
		return nil, fmt.Errorf("filmstock: no vector for %d: %w", pageID, ErrNotFound)
	}
	return c.v.nearest(q, n, map[int]bool{pageID: true}, c.rows), nil
}

// Direction is a press on the control surface.
type Direction int

const (
	Left Direction = iota
	Right
	Up
	Down
)

// An Explorer is a position in the space and the trail that led to it.
type Explorer struct {
	c     *Collection
	pos   []float32
	start []float32
	trail []int

	// Step is how far one press moves, in standard deviations of the
	// neighbourhood along that axis.
	//
	// Measured against a 1,200-film collection, as the share of a 24-cell grid
	// that changes per press and how often the centre moves in 8 presses:
	//
	//	1.5   1.7/24 new   centre moved 3 times   too slow to read as movement
	//	3.0   3.7/24       6
	//	6.0   ~6/24        7                      default
	//	12.0  11.0/24      7
	//	20.0  12.9/24      7                      little context survives a press
	//
	// The floor matters: below about 3 a press does not displace the nearest
	// record at all, so nothing visible happens and the control feels broken.
	Step float32

	// Neighbourhood is how many nearby records define the local axes. Too few
	// and the axes are noise; too many and they stop being local. It is clamped
	// to a sensible share of a small collection.
	Neighbourhood int

	// prev keeps the last axes so their SIGN can be carried forward.
	//
	// A principal component is only defined up to sign — power iteration returns
	// whichever way the data happens to lead. Left unchecked the horizontal axis
	// flips between steps, so "right" means one thing on one press and the
	// opposite on the next, and pressing left after right moves further away
	// instead of back. Aligning each new axis with the previous one is what makes
	// a direction mean the same thing twice.
	prev [2][]float32
}

// Explore starts a walk at a record.
func (c *Collection) Explore(pageID int) (*Explorer, error) {
	q, ok := c.v.Vector(pageID)
	if !ok {
		return nil, fmt.Errorf("filmstock: no vector for %d: %w", pageID, ErrNotFound)
	}
	return &Explorer{
		c: c, pos: q, start: append([]float32(nil), q...),
		trail: []int{pageID}, Step: 6, Neighbourhood: 150,
	}, nil
}

// A Cell is one film placed on the control surface.
type Cell struct {
	PageID int
	Score  float32 // similarity to the current position
	X, Y   float32 // position on the two local axes, roughly -1..1
}

// A View is what to draw: the record at the centre, and the neighbourhood laid
// out on the two axes the neighbourhood itself defines.
type View struct {
	Center  int
	Cells   []Cell
	AxisVar [2]float32 // share of local variance each axis explains

	axes  [2][]float32 // the local directions the screen axes correspond to
	sigma [2]float32   // spread of the neighbourhood along each, for step size
}

// View computes the current neighbourhood and its local axes.
func (e *Explorer) View(n int) (*View, error) {
	k := e.Neighbourhood
	if lim := max(len(e.c.rows)/3, 8); k > lim {
		k = lim
	}
	near := e.c.v.nearest(e.pos, k, nil, e.c.rows)
	if len(near) == 0 {
		return nil, fmt.Errorf("filmstock: nothing to explore: %w", ErrNotFound)
	}

	// Local PCA. The neighbourhood is centred, then the top two directions are
	// found by power iteration with deflation — a few hundred rows in 1024
	// dimensions, so forming a covariance matrix would cost far more than
	// iterating against the rows directly.
	rows := make([][]float32, len(near))
	for i, nb := range near {
		rows[i], _ = e.c.v.Vector(nb.PageID)
	}
	mean := make([]float32, e.c.v.dims)
	for _, r := range rows {
		for d, x := range r {
			mean[d] += x
		}
	}
	for d := range mean {
		mean[d] /= float32(len(rows))
	}
	for _, r := range rows {
		for d := range r {
			r[d] -= mean[d]
		}
	}
	a1, v1 := principal(rows, nil)
	a2, v2 := principal(rows, a1)
	alignSign(a1, e.prev[0])
	alignSign(a2, e.prev[1])
	e.prev = [2][]float32{append([]float32(nil), a1...), append([]float32(nil), a2...)}

	total := 0.0
	for _, r := range rows {
		for _, x := range r {
			total += float64(x) * float64(x)
		}
	}
	view := &View{Center: near[0].PageID, axes: [2][]float32{a1, a2}}
	if total > 0 {
		view.AxisVar = [2]float32{float32(v1 / total), float32(v2 / total)}
	}
	cnt := float64(len(rows))
	view.sigma = [2]float32{float32(math.Sqrt(v1 / cnt)), float32(math.Sqrt(v2 / cnt))}

	cells := make([]Cell, 0, len(near))
	var mx, my float32
	for i, nb := range near {
		x, y := dot(rows[i], a1), dot(rows[i], a2)
		mx, my = maxf(mx, absf(x)), maxf(my, absf(y))
		cells = append(cells, Cell{PageID: nb.PageID, Score: nb.Score, X: x, Y: y})
	}
	for i := range cells { // scale to roughly -1..1 so a caller can lay out a grid
		if mx > 0 {
			cells[i].X /= mx
		}
		if my > 0 {
			cells[i].Y /= my
		}
	}
	sort.Slice(cells, func(a, b int) bool { return cells[a].Score > cells[b].Score })
	if n > 0 && len(cells) > n {
		cells = cells[:n]
	}
	view.Cells = cells
	return view, nil
}

// Move applies a preference and returns the new view.
//
// A press translates the position along the corresponding local axis. That is
// precisely "more like this, less like that, without ignoring the rest": moving
// along one direction changes that trait and leaves every other coordinate where
// it was, so the films on the far side still contribute everything they have in
// common.
//
// It is NOT a pull toward the centroid of the chosen side, which is what this
// tried first. In a dense neighbourhood the centroid of either half sits almost
// on top of the centre, so a press moved the position by almost nothing and the
// same film stayed at the centre press after press.
//
// The step is measured in standard deviations of the neighbourhood along that
// axis, so it adapts: a tight cluster takes small steps, a spread-out one takes
// large ones, and neither needs a tuned constant.
func (e *Explorer) Move(d Direction, n int) (*View, error) {
	view, err := e.View(0)
	if err != nil {
		return nil, err
	}
	ax, sign := 0, float32(1)
	switch d {
	case Left:
		sign = -1
	case Right:
	case Down:
		ax, sign = 1, -1
	case Up:
		ax = 1
	}
	step := e.Step * view.sigma[ax] * sign
	for i, x := range view.axes[ax] {
		e.pos[i] += step * x
	}
	normalise(e.pos)

	next, err := e.View(n)
	if err != nil {
		return nil, err
	}
	e.trail = append(e.trail, next.Center)
	return next, nil
}

// Trail is every record that has been at the centre, oldest first.
func (e *Explorer) Trail() []int { return e.trail }

// Drift reports what the walk has been moving toward and away from.
//
// The accumulated displacement is itself a direction in the space — the trait
// the user has been steering along, which nothing ever named. Projecting the
// collection onto it and taking each end gives that direction something concrete
// to be described by.
//
// Note that embedding vectors occupy a narrow cone rather than the whole sphere,
// so the "away" end is usually still weakly positive. It is the least-toward
// end, not an opposite.
func (e *Explorer) Drift(n int) (toward, away []Neighbour) {
	dir := make([]float32, len(e.pos))
	for i := range dir {
		dir[i] = e.pos[i] - e.start[i]
	}
	if !normalise(dir) {
		return nil, nil // never moved
	}
	scored := make([]Neighbour, 0, len(e.c.rows))
	for _, i := range e.c.rows {
		v := e.c.v.row(i)
		scored = append(scored, Neighbour{PageID: int(e.c.v.ids[i]), Score: dot(v, dir)})
	}
	sort.Slice(scored, func(a, b int) bool { return scored[a].Score > scored[b].Score })
	if n <= 0 || n > len(scored) {
		n = len(scored)
	}
	toward = append(toward, scored[:n]...)
	for i := len(scored) - 1; i >= len(scored)-n; i-- {
		away = append(away, scored[i])
	}
	return toward, away
}

// principal finds the leading direction of rows, orthogonal to skip when given.
func principal(rows [][]float32, skip []float32) ([]float32, float64) {
	d := len(rows[0])
	w := make([]float32, d)
	for i := range w { // deterministic start; any non-degenerate vector works
		w[i] = float32(1 + i%7)
	}
	if skip != nil {
		orthogonalise(w, skip)
	}
	normalise(w)
	next := make([]float32, d)
	var eig float64
	for range 64 {
		for i := range next {
			next[i] = 0
		}
		for _, r := range rows {
			if p := dot(r, w); p != 0 {
				for i, x := range r {
					next[i] += x * p
				}
			}
		}
		if skip != nil {
			orthogonalise(next, skip)
		}
		eig = 0
		for _, x := range next {
			eig += float64(x) * float64(x)
		}
		if eig == 0 {
			break
		}
		if !normalise(next) {
			break
		}
		copy(w, next)
	}
	// eigenvalue: the variance captured along w
	var v float64
	for _, r := range rows {
		p := float64(dot(r, w))
		v += p * p
	}
	return w, v
}

// alignSign flips v to point the same way as ref, so a screen direction keeps
// its meaning from one step to the next.
func alignSign(v, ref []float32) {
	if ref == nil {
		// No history: pick a deterministic sign so repeated runs agree.
		var big float32
		idx := 0
		for i, x := range v {
			if absf(x) > big {
				big, idx = absf(x), i
			}
		}
		if v[idx] < 0 {
			for i := range v {
				v[i] = -v[i]
			}
		}
		return
	}
	if dot(v, ref) < 0 {
		for i := range v {
			v[i] = -v[i]
		}
	}
}

func orthogonalise(v, basis []float32) {
	p := dot(v, basis)
	for i := range v {
		v[i] -= p * basis[i]
	}
}

func dot(a, b []float32) float32 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return float32(s)
}

func normalise(v []float32) bool {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	n := float32(math.Sqrt(s))
	if n == 0 || math.IsNaN(float64(n)) {
		return false
	}
	for i := range v {
		v[i] /= n
	}
	return true
}

func absf(x float32) float32 {
	if x < 0 {
		return -x
	}
	return x
}

func maxf(a, b float32) float32 {
	if a > b {
		return a
	}
	return b
}
