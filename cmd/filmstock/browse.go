package main

import (
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"

	"github.com/szatmary/filmstock"
)

// Navigating the embedding space directly.
//
// Search asks "what matches this query"; this asks "what is near this film, and
// what happens if I keep going that way". Because every position is a point in
// the same space, a step from A to B is a DIRECTION, and continuing along it is
// meaningful: move from a war film toward a war film with a romance, extrapolate,
// and you get more romance and less war.
//
// It needs no query encoder — every vector already exists — so unlike search this
// runs on a Pi or in a browser with no model present at all.

type filmSpace struct {
	dim  int
	vecs []float32 // count x dim, L2-normalised
	ids  []int32
	byID map[int32]int
	mu   sync.Mutex
}

func loadFilmSpace(vecPath, idPath string, dim int) (*filmSpace, error) {
	vb, err := os.ReadFile(vecPath)
	if err != nil {
		return nil, err
	}
	ib, err := os.ReadFile(idPath)
	if err != nil {
		return nil, err
	}
	fs := &filmSpace{dim: dim}
	fs.vecs = make([]float32, len(vb)/4)
	for i := range fs.vecs {
		fs.vecs[i] = math.Float32frombits(binary.LittleEndian.Uint32(vb[i*4:]))
	}
	fs.ids = make([]int32, len(ib)/4)
	fs.byID = make(map[int32]int, len(fs.ids))
	for i := range fs.ids {
		fs.ids[i] = int32(binary.LittleEndian.Uint32(ib[i*4:]))
		fs.byID[fs.ids[i]] = i
	}
	if len(fs.vecs)/dim != len(fs.ids) {
		return nil, fmt.Errorf("film space: %d vectors but %d ids", len(fs.vecs)/dim, len(fs.ids))
	}
	return fs, nil
}

func (fs *filmSpace) vec(i int) []float32 { return fs.vecs[i*fs.dim : (i+1)*fs.dim] }

// nearest returns the top-k films by dot product, excluding anything in skip.
func (fs *filmSpace) nearest(q []float32, k int, skip map[int32]bool) ([]int, []float32) {
	type sc struct {
		i int
		s float32
	}
	best := make([]sc, 0, k+1)
	for i := 0; i < len(fs.ids); i++ {
		if skip[fs.ids[i]] {
			continue
		}
		v := fs.vec(i)
		var s float32
		for d := 0; d < fs.dim; d++ {
			s += v[d] * q[d]
		}
		if len(best) < k {
			best = append(best, sc{i, s})
			if len(best) == k {
				sort.Slice(best, func(a, b int) bool { return best[a].s > best[b].s })
			}
			continue
		}
		if s > best[k-1].s {
			best[k-1] = sc{i, s}
			for j := k - 1; j > 0 && best[j].s > best[j-1].s; j-- {
				best[j], best[j-1] = best[j-1], best[j]
			}
		}
	}
	if len(best) < k {
		sort.Slice(best, func(a, b int) bool { return best[a].s > best[b].s })
	}
	idx := make([]int, len(best))
	scs := make([]float32, len(best))
	for i, b := range best {
		idx[i], scs[i] = b.i, b.s
	}
	return idx, scs
}

// project2D gives the neighbours screen coordinates via a 2-component PCA of the
// local neighbourhood, so layout carries meaning: clusters group visibly and the
// direction being walked has a visible axis. A ranked list laid out in a grid
// would look the same but say nothing.
func project2D(fs *filmSpace, idx []int, center []float32) [][2]float64 {
	n := len(idx)
	out := make([][2]float64, n)
	if n < 3 {
		for i := range out {
			out[i] = [2]float64{float64(i), 0}
		}
		return out
	}
	// Centre the neighbourhood on the current position.
	rows := make([][]float64, n)
	for i, ix := range idx {
		v := fs.vec(ix)
		r := make([]float64, fs.dim)
		for d := range r {
			r[d] = float64(v[d] - center[d])
		}
		rows[i] = r
	}
	// Two power-iteration components — enough for a layout, and cheap.
	comp := make([][]float64, 2)
	rng := rand.New(rand.NewSource(1))
	for c := 0; c < 2; c++ {
		w := make([]float64, fs.dim)
		for d := range w {
			w[d] = rng.NormFloat64()
		}
		for iter := 0; iter < 12; iter++ {
			nw := make([]float64, fs.dim)
			for _, r := range rows {
				var dot float64
				for d := range r {
					dot += r[d] * w[d]
				}
				for d := range r {
					nw[d] += dot * r[d]
				}
			}
			// Orthogonalise against earlier components.
			for k := 0; k < c; k++ {
				var dot float64
				for d := range nw {
					dot += nw[d] * comp[k][d]
				}
				for d := range nw {
					nw[d] -= dot * comp[k][d]
				}
			}
			var norm float64
			for _, x := range nw {
				norm += x * x
			}
			norm = math.Sqrt(norm)
			if norm == 0 {
				break
			}
			for d := range nw {
				nw[d] /= norm
			}
			w = nw
		}
		comp[c] = w
	}
	for i, r := range rows {
		var x, y float64
		for d := range r {
			x += r[d] * comp[0][d]
			y += r[d] * comp[1][d]
		}
		out[i] = [2]float64{x, y}
	}
	return out
}

type browseHit struct {
	PageID int     `json:"page_id"`
	Title  string  `json:"title"`
	Year   string  `json:"year"`
	Score  float64 `json:"score"`
	Cover  string  `json:"cover"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
}

func (s *server) filmMeta(db *sql.DB, pid int) (string, string, string) {
	var title, cover string
	var year interface{}
	if err := db.QueryRow(
		`SELECT title, year, coalesce(cover_image_file,'') FROM movies WHERE id=?`, pid,
	).Scan(&title, &year, &cover); err != nil {
		return "", "", ""
	}
	yr := ""
	if year != nil {
		yr = fmt.Sprint(year)
	}
	return filmstock.CleanTitle(title), yr, filmstock.FilePathURL(cover, 200)
}

// handleAPINear returns the neighbourhood of a film, or of a point reached by
// walking. Query params:
//
//	id     current film
//	from   optional: the film we stepped away from — with id, defines a direction
//	alpha  how far to continue along that direction (0 = land on id, 1 = one more step)
func (s *server) handleAPINear(w http.ResponseWriter, r *http.Request) {
	if s.space == nil {
		http.Error(w, "browser not configured (-films)", 503)
		return
	}
	q := r.URL.Query()
	id, _ := strconv.Atoi(q.Get("id"))
	k := 24
	if v, err := strconv.Atoi(q.Get("k")); err == nil && v > 0 && v <= 100 {
		k = v
	}
	fs := s.space

	// No id: start somewhere. Bias toward longer articles so a random start is a
	// recognisable film rather than a stub.
	if id == 0 {
		for tries := 0; tries < 200; tries++ {
			cand := int(fs.ids[rand.Intn(len(fs.ids))])
			var n int
			s.db.QueryRow(`SELECT length(coalesce(starring,''))+length(coalesce(director,'')) FROM movies WHERE id=?`, cand).Scan(&n)
			if n > 40 {
				id = cand
				break
			}
		}
	}
	ci, ok := fs.byID[int32(id)]
	if !ok {
		http.Error(w, "unknown film", 404)
		return
	}

	pos := make([]float32, fs.dim)
	copy(pos, fs.vec(ci))

	// Directional walk: continue along the step that got us here.
	alpha := 0.0
	if v, err := strconv.ParseFloat(q.Get("alpha"), 64); err == nil {
		alpha = v
	}
	fromID, _ := strconv.Atoi(q.Get("from"))
	if alpha != 0 && fromID != 0 {
		if fi, ok := fs.byID[int32(fromID)]; ok {
			prev := fs.vec(fi)
			var norm float64
			for d := range pos {
				pos[d] += float32(alpha) * (pos[d] - prev[d])
				norm += float64(pos[d]) * float64(pos[d])
			}
			norm = math.Sqrt(norm)
			if norm > 0 {
				for d := range pos {
					pos[d] /= float32(norm)
				}
			}
		}
	}

	skip := map[int32]bool{int32(id): true}
	if fromID != 0 {
		skip[int32(fromID)] = true
	}
	idx, scores := fs.nearest(pos, k, skip)
	xy := project2D(fs, idx, pos)

	out := make([]browseHit, 0, len(idx))
	for i, ix := range idx {
		pid := int(fs.ids[ix])
		t, y, c := s.filmMeta(s.db, pid)
		if t == "" {
			continue
		}
		out = append(out, browseHit{pid, t, y, float64(scores[i]), c, xy[i][0], xy[i][1]})
	}
	ct, cy, cc := s.filmMeta(s.db, id)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"center":    browseHit{id, ct, cy, 1, cc, 0, 0},
		"neighbors": out,
	})
}

func (s *server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages.ExecuteTemplate(w, "browse.html", nil); err != nil {
		http.Error(w, err.Error(), 500)
	}
}
