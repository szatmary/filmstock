package main

import (
	"math"
	"net/http"
	"sort"

	"github.com/szatmary/filmstock"
)

// Laying out a crossroads.
//
// The four arrow targets are chosen to be the most mutually different of the
// near candidates — farthest-point sampling on the directions from the current
// position — so a step is a fork between four genuinely distinct ways to go,
// not four shades of the nearest thing. The most opposed pair faces off as
// left and right; the remaining two take up and down.
//
// Those four directions then define the screen. Every other candidate is
// placed by projecting its offset onto the left-right and up-down lines, so a
// film drawn beyond a pole is further along the direction that pole names, and
// the eye can read the grid as "keep going that way".
func (s *server) layout(r *http.Request, pos []float32, near []filmstock.Neighbour,
	cols, rows int, out *viewJSON) []cellJSON {

	// Offsets from the position, for every candidate with a vector.
	type cand struct {
		n   filmstock.Neighbour
		off []float32
	}
	cands := make([]cand, 0, len(near))
	for _, nb := range near {
		v, ok := s.ex.v.Vector(nb.PageID)
		if !ok {
			continue
		}
		off := make([]float32, len(v))
		for i := range v {
			off[i] = v[i] - pos[i]
		}
		if unit(off) {
			cands = append(cands, cand{nb, off})
		}
	}
	if len(cands) < 5 {
		return nil
	}

	// Poles by farthest-point sampling over the nearer candidates. Sampling
	// from the whole list would pick exotic outliers; the poles should be
	// plausible next steps that merely disagree with each other.
	m := min(40, len(cands))
	first, second, worst := 0, 1, float32(2)
	for i := 0; i < m; i++ {
		for j := i + 1; j < m; j++ {
			if d := dot(cands[i].off, cands[j].off); d < worst {
				first, second, worst = i, j, d
			}
		}
	}
	poles := []int{first, second}
	for len(poles) < 4 && len(poles) < m {
		best, bestD := -1, float32(2)
		for i := 0; i < m; i++ {
			taken := false
			for _, p := range poles {
				if p == i {
					taken = true
				}
			}
			if taken {
				continue
			}
			// The candidate least aligned with every chosen pole.
			maxd := float32(-2)
			for _, p := range poles {
				if d := dot(cands[i].off, cands[p].off); d > maxd {
					maxd = d
				}
			}
			if maxd < bestD {
				best, bestD = i, maxd
			}
		}
		if best < 0 {
			break
		}
		poles = append(poles, best)
	}

	// The most opposed pair is left/right; the other two are up/down.
	pL, pR := cands[poles[0]], cands[poles[1]]
	var pU, pD cand
	if len(poles) >= 4 {
		pU, pD = cands[poles[2]], cands[poles[3]]
	} else {
		pU, pD = cands[poles[0]], cands[poles[1]]
	}
	u1 := diff(pR.off, pL.off)
	unit(u1)
	u2 := diff(pU.off, pD.off)
	orthogonalise(u2, u1)
	if !unit(u2) {
		u2 = make([]float32, len(u1)) // degenerate; vertical collapses
	}
	// Up is whichever of the pair projects higher.
	if dot(pU.off, u2) < dot(pD.off, u2) {
		pU, pD = pD, pU
	}

	// Project everything, scale to the grid.
	xs := make([]float32, len(cands))
	ys := make([]float32, len(cands))
	var mx, my float32
	for i, c := range cands {
		xs[i], ys[i] = dot(c.off, u1), dot(c.off, u2)
		mx, my = maxAbs(mx, xs[i]), maxAbs(my, ys[i])
	}
	if mx == 0 {
		mx = 1
	}
	if my == 0 {
		my = 1
	}

	mid := (rows/2)*cols + cols/2
	grid := make([]*cellJSON, cols*rows)
	c := out.Center
	grid[mid] = &c

	poleAt := map[int]struct {
		c   cand
		dir string
	}{
		mid - 1:    {pL, "left"},
		mid + 1:    {pR, "right"},
		mid - cols: {pU, "up"},
		mid + cols: {pD, "down"},
	}
	placed := map[int]bool{out.Center.PageID: true}
	for slot, p := range poleAt {
		if placed[p.c.n.PageID] {
			continue
		}
		cell := s.fillCell(r, p.c.n.PageID, p.c.n.Score, 0, 0)
		cell.Pole = p.dir
		grid[slot] = &cell
		placed[p.c.n.PageID] = true
	}

	// Remaining slots from the middle outward, each taking the unplaced
	// candidate whose projection lands nearest.
	type slot struct {
		i    int
		x, y float32
		d    float32
	}
	var slots []slot
	for rr := 0; rr < rows; rr++ {
		for cc := 0; cc < cols; cc++ {
			i := rr*cols + cc
			if grid[i] != nil {
				continue
			}
			x, y := float32(0), float32(0)
			if cols > 1 {
				x = -1 + 2*float32(cc)/float32(cols-1)
			}
			if rows > 1 {
				y = 1 - 2*float32(rr)/float32(rows-1)
			}
			slots = append(slots, slot{i, x, y, x*x + y*y})
		}
	}
	sort.Slice(slots, func(a, b int) bool { return slots[a].d < slots[b].d })
	var cells []cellJSON
	for _, sl := range slots {
		best, bestD := -1, float32(math.MaxFloat32)
		for i, cd := range cands {
			if placed[cd.n.PageID] {
				continue
			}
			dx, dy := xs[i]/mx-sl.x, ys[i]/my-sl.y
			if d := dx*dx + dy*dy; d < bestD {
				best, bestD = i, d
			}
		}
		if best < 0 {
			break
		}
		placed[cands[best].n.PageID] = true
		cell := s.fillCell(r, cands[best].n.PageID, cands[best].n.Score,
			xs[best]/mx, ys[best]/my)
		grid[sl.i] = &cell
		cells = append(cells, cell)
	}
	out.Grid = grid
	// Axis naming wants every candidate's projection, placed or not.
	all := make([]cellJSON, 0, len(cands))
	for i, cd := range cands {
		cj := s.fillCell(r, cd.n.PageID, cd.n.Score, xs[i]/mx, ys[i]/my)
		all = append(all, cj)
	}
	return all
}

func facetCounts(cells []cellJSON) (map[string]int, int) {
	m := map[string]int{}
	for _, c := range cells {
		seen := map[string]bool{}
		for _, a := range attrsOf(c, c.genre, c.country, c.language) {
			if !seen[a] {
				seen[a] = true
				m[a]++
			}
		}
	}
	return m, len(cells)
}

func (s *server) nameAxes(cells []cellJSON, out *viewJSON) {
	facets := map[int][]string{}
	for _, c := range cells {
		facets[c.PageID] = attrsOf(c, c.genre, c.country, c.language)
	}
	split := func(axis func(cellJSON) float32) (lo, hi []cellJSON) {
		for _, c := range cells {
			switch v := axis(c); {
			case v < -0.33:
				lo = append(lo, c)
			case v > 0.33:
				hi = append(hi, c)
			}
		}
		return
	}
	lo, hi := split(func(c cellJSON) float32 { return c.X })
	l, h := describeEnds(lo, hi, facets)
	out.AxisNames.LeftEnd, out.AxisNames.RightEnd = axisLabel(l), axisLabel(h)
	lo, hi = split(func(c cellJSON) float32 { return c.Y })
	l, h = describeEnds(lo, hi, facets)
	out.AxisNames.DownEnd, out.AxisNames.UpEnd = axisLabel(l), axisLabel(h)
}

func dot(a, b []float32) float32 {
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

func diff(a, b []float32) []float32 {
	out := make([]float32, len(a))
	for i := range a {
		out[i] = a[i] - b[i]
	}
	return out
}

func unit(v []float32) bool {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	if n < 1e-12 {
		return false
	}
	f := float32(1 / math.Sqrt(n))
	for i := range v {
		v[i] *= f
	}
	return true
}

func orthogonalise(v, basis []float32) {
	d := dot(v, basis)
	for i := range v {
		v[i] -= d * basis[i]
	}
}

func maxAbs(cur, v float32) float32 {
	if v < 0 {
		v = -v
	}
	if v > cur {
		return v
	}
	return cur
}
