// The explorer: zeroing in on a taste by choosing at crossroads.
//
// The centre is a STACK of selections, not a single film. Every film chosen
// goes on top, and the sum of the stack's vectors is a position in the space —
// a taste that is no single record. The neighbourhood of that position fills
// the grid.
//
// The four arrow targets are not the four nearest films. They are the four
// most POLAR of the near candidates — chosen to be maximally different from
// one another — so every step is a fork between genuinely distinct directions
// rather than four shades of the same thing. The most opposed pair faces off
// as left/right, the other two as up/down, and the rest of the grid is filled
// by projecting each candidate onto those two lines: the films beyond a pole
// are further along the direction it names. Choosing repeatedly at these
// crossroads is how a user zeroes in.
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/szatmary/filmstock"
)

type walk struct {
	stack      []int // every selection, newest last; the sum is the position
	grid       []*cellJSON
	cols, rows int

	prevFacets map[string]int
	prevN      int
}

type explorers struct {
	mu    sync.Mutex
	v     *filmstock.Vectors
	coll  *filmstock.Collection
	known []int // films in both the database and the vector file
	byID  map[string]*walk
	next  int
}

func newExplorers(v *filmstock.Vectors, known []int) *explorers {
	coll, missing := v.Collect(known)
	fmt.Fprintf(os.Stderr, "explorer: %d films with vectors (%d in the database have none)\n",
		coll.Len(), missing)
	usable := make([]int, 0, len(known))
	for _, id := range known {
		if _, ok := v.Vector(id); ok {
			usable = append(usable, id)
		}
	}
	return &explorers{v: v, coll: coll, known: usable, byID: map[string]*walk{}}
}

type cellJSON struct {
	PageID int     `json:"page_id"`
	Title  string  `json:"title"`
	Year   int     `json:"year"`
	Poster string  `json:"poster,omitempty"`
	Score  float32 `json:"score"`
	X      float32 `json:"x"`
	Y      float32 `json:"y"`
	Centre bool    `json:"centre,omitempty"`
	Pole   string  `json:"pole,omitempty"` // left/right/up/down when an arrow target

	genre, country, language string
}

type viewJSON struct {
	Session string      `json:"session"`
	Center  cellJSON    `json:"center"`
	Cols    int         `json:"cols"`
	Rows    int         `json:"rows"`
	Grid    []*cellJSON `json:"grid"`
	Step    struct {
		Gained   []string `json:"gained"`
		Lost     []string `json:"lost"`
		Only     []string `json:"only"`
		Shared   []string `json:"shared"`
		Rejected []string `json:"rejected"`
	} `json:"step"`
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

// position is the sum of the stack's vectors, normalised. A taste, not a film.
func (e *explorers) position(stack []int) ([]float32, bool) {
	var pos []float32
	for _, id := range stack {
		v, ok := e.v.Vector(id)
		if !ok {
			continue
		}
		if pos == nil {
			pos = make([]float32, len(v))
		}
		for i, x := range v {
			pos[i] += x
		}
	}
	if pos == nil {
		return nil, false
	}
	var n float64
	for _, x := range pos {
		n += float64(x) * float64(x)
	}
	if n == 0 {
		return nil, false
	}
	f := float32(1 / math.Sqrt(n))
	for i := range pos {
		pos[i] *= f
	}
	return pos, true
}

func (s *server) handleAPIExplore(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cols, rows := gridDims(q)
	s.ex.mu.Lock()
	defer s.ex.mu.Unlock()

	session := q.Get("s")
	wk := s.ex.byID[session]
	var offered map[string]int
	if wk != nil {
		offered = wk.candidates()
	}
	switch {
	case q.Get("start") != "" || wk == nil:
		var id int
		if st := q.Get("start"); st == "random" || (st == "" && wk == nil) {
			// A dealt first card, not an empty page. Drawn until one has a
			// poster: a blank rectangle is a poor opening move.
			for range 30 {
				id = s.ex.known[rand.IntN(len(s.ex.known))]
				if c := s.fillCell(r, id, 0, 0, 0); c.Poster != "" {
					break
				}
			}
		} else {
			var err error
			if id, err = strconv.Atoi(st); err != nil {
				http.Error(w, "unknown session; start again", 400)
				return
			}
		}
		s.ex.next++
		session = strconv.Itoa(s.ex.next)
		wk = &walk{stack: []int{id}}
		s.ex.byID[session] = wk
	case q.Get("pop") != "":
		// Clicking a card in the stack unwinds to that pick: everything chosen
		// after it comes off, and the sum reverts to what it was at that
		// moment. Backspace is the one-step case of this.
		if i, err := strconv.Atoi(q.Get("pop")); err == nil && i >= 0 && i < len(wk.stack)-1 {
			wk.stack = wk.stack[:i+1]
		}
	case q.Get("goto") != "":
		if id, err := strconv.Atoi(q.Get("goto")); err == nil && id != wk.top() {
			wk.stack = append(wk.stack, id)
		}
	case q.Get("move") == "back":
		if len(wk.stack) > 1 {
			wk.stack = wk.stack[:len(wk.stack)-1]
		}
	case q.Get("move") != "":
		if next, ok := wk.neighbour(q.Get("move")); ok && next != wk.top() {
			wk.stack = append(wk.stack, next)
		}
	}

	pos, ok := s.ex.position(wk.stack)
	if !ok {
		http.Error(w, "no vectors for the selection", 404)
		return
	}
	// Enough candidates that the projections have spread beyond the grid.
	near := s.ex.coll.Nearest(pos, cols*rows*3, wk.stack)
	if len(near) < 5 {
		http.Error(w, "nothing to explore here", 500)
		return
	}

	out := viewJSON{Session: session, Cols: cols, Rows: rows}
	out.Center = s.fillCell(r, wk.top(), 1, 0, 0)
	out.Center.Centre = true
	cells := s.layout(r, pos, near, cols, rows, &out)
	wk.grid, wk.cols, wk.rows = out.Grid, cols, rows

	s.nameAxes(cells, &out)
	now, nNow := facetCounts(cells)
	if wk.prevFacets != nil {
		out.Step.Gained, out.Step.Lost = describeStep(wk.prevFacets, now, wk.prevN, nNow)
	}
	wk.prevFacets, wk.prevN = now, nNow
	if len(offered) > 1 {
		var rejected [][]string
		for _, id := range sortedIDs(offered) {
			if id == wk.top() {
				continue
			}
			c := s.fillCell(r, id, 0, 0, 0)
			rejected = append(rejected, attrsOf(c, c.genre, c.country, c.language))
			out.Step.Rejected = append(out.Step.Rejected, c.Title)
		}
		if len(rejected) > 0 {
			cc := out.Center
			out.Step.Only, out.Step.Shared = describeChoice(
				attrsOf(cc, cc.genre, cc.country, cc.language), rejected)
		}
	}
	out.Steps = len(wk.stack) - 1
	for _, id := range wk.stack {
		out.Trail = append(out.Trail, s.fillCell(r, id, 0, 0, 0))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (wk *walk) top() int { return wk.stack[len(wk.stack)-1] }

// candidates lists what the four arrows currently offer: the poles.
func (wk *walk) candidates() map[string]int {
	out := map[string]int{}
	for _, d := range []string{"left", "right", "up", "down"} {
		if id, ok := wk.neighbour(d); ok {
			out[d] = id
		}
	}
	return out
}

// neighbour reads the pole pinned beside the middle, walking outward past any
// empty slot.
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

func sortedIDs(m map[string]int) []int {
	var out []int
	for _, d := range []string{"left", "right", "up", "down"} {
		if id, ok := m[d]; ok {
			out = append(out, id)
		}
	}
	return out
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

// handleAPISynopsis returns what the record says about one film, for the
// overlay the explorer opens on Enter. The grid answers "what is near";
// this answers "what actually is it" without leaving the walk.
func (s *server) handleAPISynopsis(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.URL.Query().Get("id"))
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	m, err := s.fs.Film(r.Context(), id)
	if err != nil {
		http.Error(w, "not found", 404)
		return
	}
	year := ""
	if len(m.ReleaseDates) > 0 && len(m.ReleaseDates[0]) >= 4 {
		year = m.ReleaseDates[0][:4]
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"title": m.Title, "year": year,
		"genre":    strings.Join(m.Genre, " · "),
		"director": strings.Join(personNames(m.Director), " · "),
		"starring": strings.Join(personNames(m.Starring), " · "),
		"runtime":  m.Runtime,
		"overview": m.Overview,
		"plot":     m.Plot,
		"poster":   m.CoverImageURL,
		"url":      m.WikiURL,
	})
}

func personNames(ps []filmstock.Person) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}
