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

// describeStep says what changed by moving here.
//
// The axis labels describe the NEIGHBOURHOOD, and that is the wrong instrument
// for judging a step. Moving from Star Trek to Galaxy Quest, both sit among
// spaceships, so "science fiction" dominates both descriptions and the thing
// that actually distinguishes them — that one is a comedy — never surfaces. The
// shared context is exactly what should be subtracted out.
//
// So a step is described by what is over-represented around where you ARE and
// not around where you WERE, and the reverse. What every film in both places
// has in common cancels, which is the point: it is what the two have in common
// that makes the move invisible.
func describeStep(before, after map[string]int, nBefore, nAfter int) (gained, lost []string) {
	share := func(m map[string]int, k string, n int) float64 {
		if n == 0 {
			return 0
		}
		return float64(m[k]) / float64(n)
	}
	type kv struct {
		k string
		d float64
	}
	var up, down []kv
	seen := map[string]bool{}
	for _, m := range []map[string]int{before, after} {
		for k := range m {
			if seen[k] {
				continue
			}
			seen[k] = true
			d := share(after, k, nAfter) - share(before, k, nBefore)
			// A term needs a real presence somewhere, not just a swing: two
			// films appearing or vanishing is noise at this size.
			switch {
			case d > 0.12 && after[k] >= 3:
				up = append(up, kv{k, d})
			case d < -0.12 && before[k] >= 3:
				down = append(down, kv{k, -d})
			}
		}
	}
	pick := func(xs []kv) []string {
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
	return pick(up), pick(down)
}

// describeChoice says what set the film you took apart from the ones you did
// not.
//
// A step has four candidates — the films up, down, left and right — and picking
// one is a preference expressed against three specific alternatives, not
// against the corpus or the neighbourhood. That is a much narrower and more
// honest contrast: "gained Comedy" says the region changed, while "Comedy,
// where the others were Drama, Thriller and Horror" says why THIS one.
//
// An attribute counts as distinguishing when the chosen film has it and few of
// the rejected do. Something all four share explains nothing about the choice.
func describeChoice(chosen []string, rejected [][]string) (only []string, shared []string) {
	has := map[string]int{}
	for _, r := range rejected {
		seen := map[string]bool{}
		for _, a := range r {
			if !seen[a] {
				seen[a] = true
				has[a]++
			}
		}
	}
	seen := map[string]bool{}
	for _, a := range chosen {
		if seen[a] {
			continue
		}
		seen[a] = true
		switch {
		case has[a] == 0:
			only = append(only, a)
		case has[a] == len(rejected) && len(rejected) > 1:
			shared = append(shared, a)
		}
	}
	sort.Strings(only)
	sort.Strings(shared)
	if len(only) > 4 {
		only = only[:4]
	}
	if len(shared) > 4 {
		shared = shared[:4]
	}
	return only, shared
}
