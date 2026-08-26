// The explorer: navigating the embedding space by hand.
//
// filmstock.Explorer places a neighbourhood on two axes that the neighbourhood
// itself defines, and moves along them. There is no way to judge whether those
// axes mean anything from test output — the question is whether the films to
// the right of a film feel like a direction, and that is answered by looking.
// So this exists to look.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/szatmary/filmstock"
)

// A walk is a film you are standing on and how you got there.
//
// The arrows step from film to film, not through the space. Moving a POINT by
// some distance and taking whatever lands nearest means you cannot tell where a
// press will put you — the film to your right is not necessarily the one you
// arrive at. Stepping to the film you can see makes the control honest: what is
// two to the right is what two presses reach.
//
// The last grid is kept because that is what "to the right" refers to. It is a
// property of the layout the user is looking at, not of the vector space.
type walk struct {
	cur        int
	trail      []int
	grid       []*cellJSON
	cols, rows int
}

type explorers struct {
	mu   sync.Mutex
	coll *filmstock.Collection
	byID map[string]*walk
	next int
}

// newExplorers restricts the space to films the database can name.
//
// The vector file is built from a corpus of its own, and 3% of its ids no
// longer resolve to anything — 5,199 of 170,421. That sounds negligible and is
// not: they cluster, so three steps in one direction lands in a region where
// every cell is a bare "#79638163" and the walk becomes unreadable. Exploring
// only what can be drawn keeps the surface meaningful the whole way out.
func newExplorers(v *filmstock.Vectors, known []int) *explorers {
	coll, missing := v.Collect(known)
	fmt.Fprintf(os.Stderr, "explorer: %d films with vectors (%d in the database have none)\n",
		coll.Len(), missing)
	return &explorers{coll: coll, byID: map[string]*walk{}}
}

// cellJSON is one film on the surface: where the axes put it, and enough to
// draw it.
type cellJSON struct {
	PageID int     `json:"page_id"`
	Title  string  `json:"title"`
	Year   int     `json:"year"`
	Poster string  `json:"poster,omitempty"`
	Score  float32 `json:"score"`
	X      float32 `json:"x"`
	Y      float32 `json:"y"`
	Centre bool    `json:"centre,omitempty"`

	genre, country, language string // for naming the axes, not for the client
}

type viewJSON struct {
	Session string      `json:"session"`
	Center  cellJSON    `json:"center"`
	Cols    int         `json:"cols"`
	Rows    int         `json:"rows"`
	Grid    []*cellJSON `json:"grid"` // row-major, cols*rows, nil where empty
	AxisVar [2]float32  `json:"axis_var"`
	// What the two directions appear to MEAN here, read off the films at each
	// end. Latent axes have no given name; this is the nearest honest thing.
	AxisNames struct {
		LeftEnd  string `json:"left"`
		RightEnd string `json:"right"`
		DownEnd  string `json:"down"`
		UpEnd    string `json:"up"`
	} `json:"axis_names"`
	Trail []cellJSON `json:"trail"`
	Steps int        `json:"steps"`
}

func (s *server) fillCell(r *http.Request, pageID int, score, x, y float32) cellJSON {
	c := cellJSON{PageID: pageID, Score: score, X: x, Y: y}
	if m, err := s.fs.Film(r.Context(), pageID); err == nil {
		c.Title, c.Poster = m.Title, m.CoverImageURL
		if len(m.ReleaseDates) > 0 && len(m.ReleaseDates[0]) >= 4 {
			c.Year, _ = strconv.Atoi(m.ReleaseDates[0][:4])
		}
		c.genre = strings.Join(m.Genre, " · ")
		c.country = strings.Join(filmstock.Names(m.Country), " · ")
		c.language = strings.Join(filmstock.Names(m.Language), " · ")
	}
	if c.Title == "" {
		c.Title = fmt.Sprintf("#%d", pageID)
	}
	return c
}

// handleAPIExplore starts a walk or moves one.
//
// The explorer is kept server-side and addressed by a session id, because a
// walk is a position AND the trail that produced it — the axes are aligned
// against the previous step's, so "right" only means the same thing twice if
// the previous step is still there. A stateless API would lose exactly the
// property the feature exists to provide.
func (s *server) handleAPIExplore(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cols, rows := gridDims(q)
	n, _ := strconv.Atoi(q.Get("n"))
	if n < cols*rows+8 {
		n = cols*rows + 8
	}
	s.ex.mu.Lock()
	defer s.ex.mu.Unlock()

	session := q.Get("s")
	wk := s.ex.byID[session]
	if start := q.Get("start"); start != "" || wk == nil {
		id, err := strconv.Atoi(start)
		if err != nil {
			http.Error(w, "unknown session; start again", 400)
			return
		}
		s.ex.next++
		session = strconv.Itoa(s.ex.next)
		wk = &walk{cur: id, trail: []int{id}}
		s.ex.byID[session] = wk
	} else if dir := q.Get("move"); dir == "back" {
		// Left is not the inverse of right, and cannot be: the grid is
		// recomputed at each film, so the film that was to your right is not
		// where "left" points once you are standing on it. Going back needs the
		// trail, which is the only record of where you actually came from.
		if len(wk.trail) > 1 {
			wk.trail = wk.trail[:len(wk.trail)-1]
			wk.cur = wk.trail[len(wk.trail)-1]
		}
	} else if dir != "" {
		next, ok := wk.neighbour(dir)
		if !ok {
			// Nothing that way: the edge of the grid, or an empty slot with no
			// film beyond it. Redraw where we are rather than pretending.
			next = wk.cur
		}
		if next != wk.cur {
			wk.cur = next
			wk.trail = append(wk.trail, next)
		}
	}

	// The axes are recomputed at the film we are standing on, every time.
	e, err := s.ex.coll.Explore(wk.cur)
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	view, err := e.View(n)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	out := viewJSON{Session: session, AxisVar: view.AxisVar, Cols: cols, Rows: rows}
	out.Center = s.fillCell(r, view.Center, 1, 0, 0)
	out.Center.Centre = true
	for _, row := range view.Grid(cols, rows) {
		for _, c := range row {
			if c == nil {
				out.Grid = append(out.Grid, nil)
				continue
			}
			cell := s.fillCell(r, c.PageID, c.Score, c.X, c.Y)
			out.Grid = append(out.Grid, &cell)
		}
	}
	// The film we are standing on takes the middle slot, always: without it the
	// arrows have no cursor to move.
	if mid := (rows/2)*cols + cols/2; mid < len(out.Grid) {
		at := -1
		for i, c := range out.Grid {
			if c != nil && c.PageID == out.Center.PageID {
				at = i
				break
			}
		}
		switch {
		case at == mid:
			out.Grid[mid].Centre = true
		case at >= 0:
			out.Grid[at], out.Grid[mid] = out.Grid[mid], out.Grid[at]
			out.Grid[mid].Centre = true
		default:
			c := out.Center
			out.Grid[mid] = &c
		}
	}
	wk.grid, wk.cols, wk.rows = out.Grid, cols, rows

	s.nameAxes(r, view, &out)
	out.Steps = len(wk.trail) - 1
	for _, id := range wk.trail {
		out.Trail = append(out.Trail, s.fillCell(r, id, 0, 0, 0))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// neighbour returns the film one slot along from the middle.
//
// If that slot is empty it keeps going in the same direction rather than
// stopping, so a gap in the layout does not make the control feel broken.
func (wk *walk) neighbour(dir string) (int, bool) {
	if wk.grid == nil || wk.cols == 0 {
		return 0, false
	}
	r0, c0 := wk.rows/2, wk.cols/2
	dr, dc := 0, 0
	switch dir {
	case "left":
		dc = -1
	case "right":
		dc = 1
	case "up":
		dr = -1
	case "down":
		dr = 1
	default:
		return 0, false
	}
	for r, c := r0+dr, c0+dc; r >= 0 && r < wk.rows && c >= 0 && c < wk.cols; r, c = r+dr, c+dc {
		if cell := wk.grid[r*wk.cols+c]; cell != nil {
			return cell.PageID, true
		}
	}
	return 0, false
}

func gridDims(q map[string][]string) (int, int) {
	get := func(k string, def, lo, hi int) int {
		if v, ok := q[k]; ok && len(v) > 0 {
			if n, err := strconv.Atoi(v[0]); err == nil && n >= lo && n <= hi {
				return n
			}
		}
		return def
	}
	cols, rows := get("cols", 7, 3, 11), get("rows", 5, 3, 7)
	if cols%2 == 0 {
		cols--
	}
	if rows%2 == 0 {
		rows--
	}
	return cols, rows
}

func (s *server) handleExplore(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages.ExecuteTemplate(w, "explore.html", r.URL.Query().Get("id")); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

// nameAxes reads what the two directions mean here off the films at their ends.
//
// Every cell in the neighbourhood, not only the ones that won a grid slot: the
// grid is a display choice, and the axis means what it means regardless of how
// many squares happened to be free.
func (s *server) nameAxes(r *http.Request, view *filmstock.View, out *viewJSON) {
	all := make([]cellJSON, 0, len(view.Cells))
	facets := map[int][]string{}
	for _, c := range view.Cells {
		cj := s.fillCell(r, c.PageID, c.Score, c.X, c.Y)
		all = append(all, cj)
		facets[cj.PageID] = attrsOf(cj, cj.genre, cj.country, cj.language)
	}
	split := func(axis func(cellJSON) float32) (lo, hi []cellJSON) {
		for _, c := range all {
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
