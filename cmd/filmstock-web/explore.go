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

type explorers struct {
	mu   sync.Mutex
	coll *filmstock.Collection
	byID map[string]*filmstock.Explorer
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
	return &explorers{coll: coll, byID: map[string]*filmstock.Explorer{}}
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
	n, _ := strconv.Atoi(q.Get("n"))
	if n <= 0 {
		n = 24
	}
	s.ex.mu.Lock()
	defer s.ex.mu.Unlock()

	var e *filmstock.Explorer
	session := q.Get("s")
	switch {
	case q.Get("start") != "":
		id, err := strconv.Atoi(q.Get("start"))
		if err != nil {
			http.Error(w, "bad start id", 400)
			return
		}
		e, err = s.ex.coll.Explore(id)
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		s.ex.next++
		session = strconv.Itoa(s.ex.next)
		s.ex.byID[session] = e
	default:
		e = s.ex.byID[session]
		if e == nil {
			http.Error(w, "unknown session; start again", 400)
			return
		}
	}

	var view *filmstock.View
	var err error
	switch q.Get("move") {
	case "left":
		view, err = e.Move(filmstock.Left, n)
	case "right":
		view, err = e.Move(filmstock.Right, n)
	case "up":
		view, err = e.Move(filmstock.Up, n)
	case "down":
		view, err = e.Move(filmstock.Down, n)
	default:
		view, err = e.View(n)
	}
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// A grid, not a scatter. The axes are continuous and two records often want
	// the same corner, so drawn at their raw positions they overlap in clumps
	// and leave holes — which is unreadable, and worse, the eye cannot follow a
	// title from one press to the next. Grid assigns each slot the nearest
	// unplaced record, working outward from the middle.
	// Odd on both sides, so there is a true middle slot for the current
	// position to occupy.
	cols, rows := 7, 5
	if v := q.Get("cols"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 12 {
			cols = n
		}
	}
	if v := q.Get("rows"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 8 {
			rows = n
		}
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
	// The current position goes in the middle slot, always.
	//
	// Without it the arrows have nothing to act on: the grid simply becomes a
	// different grid and there is no cursor, no anchor, no sense of having
	// moved rather than reloaded. Where the centre already won a slot elsewhere
	// it is swapped with whatever holds the middle, so nothing is lost and the
	// middle is always where you are.
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
	// Name the axes from the neighbourhood, using every cell rather than only
	// the ones that won a grid slot: the grid is a display choice and the axis
	// means what it means regardless of how many squares are free.
	{
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

	trail := e.Trail()
	out.Steps = len(trail) - 1
	for _, id := range trail {
		out.Trail = append(out.Trail, s.fillCell(r, id, 0, 0, 0))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (s *server) handleExplore(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages.ExecuteTemplate(w, "explore.html", r.URL.Query().Get("id")); err != nil {
		http.Error(w, err.Error(), 500)
	}
}
