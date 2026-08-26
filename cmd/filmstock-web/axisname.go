package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Naming an axis.
//
// The axes are latent: nobody labelled them, and they are recomputed at every
// position, so they have no fixed meaning to look up. But they are still
// directions through real films, and what a direction MEANS can be read off the
// films at its two ends — if everything to the right is Italian and everything
// to the left is American, the axis is carrying nationality whether anyone
// named it or not.
//
// So each end is described by the attributes over-represented there COMPARED
// WITH THE OTHER END, which is the only comparison that says anything. Scoring
// against the whole corpus instead would return "Drama" for every axis of every
// neighbourhood, because most films are dramas.
type axisEnd struct {
	Terms []string `json:"terms"`
}

// attrs are the facets worth naming an axis with. Cast and crew are left out
// deliberately: a shared actor explains one pair of films, never a direction.
func attrsOf(c cellJSON, genre, country, language string) []string {
	var out []string
	add := func(prefix, v string) {
		for _, p := range strings.Split(v, "·") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, prefix+p)
			}
		}
	}
	add("", genre)
	add("", country)
	add("", language)
	if c.Year > 0 {
		out = append(out, strconv.Itoa(c.Year/10*10)+"s")
	}
	return out
}

// describeEnds names the two ends of one axis.
//
// The ends are the outer thirds along that axis. Terms are ranked by how much
// more common they are at one end than the other, with a floor on the raw count
// so a single film cannot name a direction on its own.
func describeEnds(low, high []cellJSON, facets map[int][]string) (lo, hi []string) {
	count := func(cells []cellJSON) map[string]int {
		m := map[string]int{}
		for _, c := range cells {
			seen := map[string]bool{}
			for _, a := range facets[c.PageID] {
				if !seen[a] {
					seen[a] = true
					m[a]++
				}
			}
		}
		return m
	}
	a, b := count(low), count(high)
	rank := func(mine, theirs map[string]int, n int) []string {
		type kv struct {
			k string
			d float64
		}
		var xs []kv
		for k, v := range mine {
			if v < 2 {
				continue // one film is a coincidence, not a direction
			}
			mineShare := float64(v) / float64(max(n, 1))
			theirShare := float64(theirs[k]) / float64(max(len(mine), 1))
			if d := mineShare - theirShare; d > 0.08 {
				xs = append(xs, kv{k, d})
			}
		}
		sort.Slice(xs, func(i, j int) bool {
			if xs[i].d != xs[j].d {
				return xs[i].d > xs[j].d
			}
			return xs[i].k < xs[j].k
		})
		out := make([]string, 0, 3)
		for i := 0; i < len(xs) && i < 3; i++ {
			out = append(out, xs[i].k)
		}
		return out
	}
	return rank(a, b, len(low)), rank(b, a, len(high))
}

// axisLabel renders one end for display.
func axisLabel(terms []string) string {
	if len(terms) == 0 {
		return "—"
	}
	return strings.Join(terms, " · ")
}

var _ = fmt.Sprintf
