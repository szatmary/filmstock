package build

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// The builds catalog: the one file that lists every release.
//
// A consumer arrives knowing nothing. builds.json tells them what exists and
// how it chains: fulls that stand alone, dailies that each name the build they
// apply on top of, and — at every full — the bridge that carries a
// chain-following consumer onto the fresh rebuild. The chain is explicit
// because implicit chains (date arithmetic, filename conventions) break the
// first time a day is skipped or a dump is late.
//
//	/filmstock/builds.json          this catalog
//	/filmstock/<id>/manifest.json   per-build files and hashes
//
// The bridge_size on a full is the statement count of the diff between
// [previous full + every daily] and [this full, rebuilt from scratch]. It is
// published because it is worth seeing, but it is a measurement and not a
// test: its magnitude is dominated by legitimate repair. Adds-changes dumps
// age out after about 42 days, and a full is what closes any gap the dailies
// could not — 20260802–20260817 aged out exactly this way — so a healthy
// bridge can carry weeks of edits and be entirely correct.
//
// What is actually enforced is structural and lives in guardRemovals: a full
// may repair anything, but it may only remove a record whose page is gone from
// the dump it was built from. See docs/CHAIN.md §3.
type buildsCatalog struct {
	Updated    string       `json:"updated"`
	LatestFull string       `json:"latest_full,omitempty"`
	Latest     string       `json:"latest,omitempty"`
	Builds     []buildEntry `json:"builds"`
}

type buildEntry struct {
	ID   string `json:"id"`   // opaque and unique; never parsed for meaning
	Kind string `json:"kind"` // full | daily
	Dump string `json:"dump"` // the Wikimedia dump it mirrors, 1:1
	// Through is the build's content day: the last adds-changes day applied,
	// the intermediate's incr_through. It is what orders the chain in time.
	//
	// The id used to carry this meaning implicitly, by being a date. That
	// conflated three separate facts — the release label, the dump derived
	// from, and the day the content actually reaches — and the conflation is
	// what let a full built from the 20260901 dump be a candidate for
	// publication onto a chain already at 20260902: nothing recorded that its
	// content stopped three days earlier, so nothing could compare them. See
	// docs/CHAIN.md.
	Through  string `json:"through"`
	Parent   string `json:"parent,omitempty"` // the build this applies on top of
	Manifest string `json:"manifest"`         // path to the build's manifest
	// Full builds only: the reconciliation from the previous chain.
	BridgeFrom string `json:"bridge_from,omitempty"` // last daily of the old chain
	Bridge     string `json:"bridge,omitempty"`      // path to the bridge patch
	BridgeSize int    `json:"bridge_statements,omitempty"`
	// Every route into this build, cheapest-first being the consumer's problem
	// rather than ours. Edges[0] is always the default one (the parent, or the
	// bridge on a full); the rest are rollups spanning further back.
	Edges []patchEdge `json:"edges,omitempty"`
	// Fulls only: the bytes a consumer downloads to take the full road. It is
	// the alternative every path search is measured against.
	Bytes int64 `json:"bytes,omitempty"`
}

// patchEdge is one route into a build: apply the patches named by Suffix to the
// build named by From, and you have this one.
//
// The chain used to be a line — each build naming the single build it applies
// to — and a line is why a consumer who was away six months had to walk six
// bridges and a hundred and eighty dailies or give up and refetch a full. That
// cost grows linearly with time away and never improves.
//
// Rollups make it a DAG: the same information, more routes through it. Nothing
// prescribes which route to take, because the catalog cannot know what a given
// consumer already holds. Bytes is recorded so the consumer can choose, and
// choosing by total bytes gives the right answer for every case without anyone
// enumerating the cases: a fresh install finds the full cheapest, a daily
// follower finds one patch, a returning consumer finds the rollups, and one away
// so long that no path beats refetching gets exactly that.
//
// A missing route is not a failure, only a longer path — which is what makes
// pruning old builds safe.
type patchEdge struct {
	From string `json:"from"`
	// Suffix distinguishes this build's patch files: "" is the default
	// (<db>.patch.sql.gz or <db>.bridge.sql.gz), and a rollup adds
	// ".from-<id>" so several routes can live in one build directory.
	Suffix string `json:"suffix,omitempty"`
	Bytes  int64  `json:"bytes"`
}

// orderCatalog sorts the chain and recomputes the tip.
//
// The order is by content day, not by id. Sorting by id was ordering the chain
// by parsing a label as a date, which is the same mistake as keying a film on
// its title: it works only for as long as every label happens to be a
// well-formed, unique, monotonic date — and it stops working silently rather
// than loudly. `through` is the fact that was meant all along.
//
// The id breaks ties so the order is total and stable: two builds may legally
// reach the same content day (a full and the daily it supersedes), and a
// catalog whose order depends on insertion sequence is not reproducible.
func orderCatalog(cat *buildsCatalog) {
	sort.Slice(cat.Builds, func(i, j int) bool {
		a, b := cat.Builds[i], cat.Builds[j]
		if a.Through != b.Through {
			return a.Through < b.Through
		}
		return a.ID < b.ID
	})
	cat.Updated = time.Now().UTC().Format(time.RFC3339)
	cat.Latest, cat.LatestFull = "", ""
	for _, x := range cat.Builds {
		cat.Latest = x.ID
		if x.Kind == "full" {
			cat.LatestFull = x.ID
		}
	}
}

// throughOf reports a build's content day, and whether the catalog knows it.
func throughOf(cat buildsCatalog, id string) (string, bool) {
	for _, x := range cat.Builds {
		if x.ID == id {
			return x.Through, x.Through != ""
		}
	}
	return "", false
}

// backfillThrough is the one-time migration for entries published before
// `through` existed. Those ids were dates and meant the content day, so that
// is what they become — an assertion about what was already true, made once,
// rather than a fallback consulted at runtime.
func backfillThrough(path string) {
	var cat buildsCatalog
	b, err := os.ReadFile(path)
	if err != nil {
		fatal(err)
	}
	if err := json.Unmarshal(b, &cat); err != nil {
		fatal(err)
	}
	n := 0
	for i, x := range cat.Builds {
		if x.Through != "" {
			continue
		}
		if len(x.ID) != 8 {
			fatal(fmt.Errorf("build %q predates `through` but its id is not a date, "+
				"so its content day cannot be recovered; set it by hand", x.ID))
		}
		cat.Builds[i].Through = x.ID
		if cat.Builds[i].Dump == "" {
			cat.Builds[i].Dump = x.ID
		}
		n++
	}
	orderCatalog(&cat)
	out, _ := json.MarshalIndent(cat, "", "  ")
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "  %s: backfilled through on %d of %d builds; latest %s\n",
		path, n, len(cat.Builds), cat.Latest)
}

// CmdBuilds adds or updates one entry in the catalog.
func CmdBuilds(args []string) {
	fs := flag.NewFlagSet("builds", flag.ExitOnError)
	catalog := fs.String("catalog", "bucket/builds.json", "the catalog to maintain")
	id := fs.String("id", "", "build id (YYYYMMDD)")
	kind := fs.String("kind", "", "full or daily")
	dump := fs.String("dump", "", "the Wikimedia dump this mirrors (default: the id)")
	parent := fs.String("parent", "", "build this applies on top of (required for daily)")
	through := fs.String("through", "", "content day: the last adds-changes day applied (YYYYMMDD)")
	bridgeFrom := fs.String("bridge-from", "", "full only: last daily of the previous chain")
	bridge := fs.String("bridge", "", "full only: path to the bridge patch")
	bridgeSize := fs.Int("bridge-statements", -1, "full only: statement count of the bridge")
	backfill := fs.Bool("backfill-through", false, "one-time migration: give pre-`through` entries through=id")
	backfillE := fs.String("backfill-edges", "", "one-time migration: price each build's existing route from the patch files in this release root")
	edgesJSON := fs.String("edges", "", "JSON array of the routes into this build")
	bytes := fs.Int64("bytes", 0, "full only: total hosted database bytes, the full-road cost")
	fs.Parse(args)
	if *backfill {
		backfillThrough(*catalog)
		return
	}
	if *backfillE != "" {
		backfillEdges(*catalog, *backfillE)
		return
	}
	if *id == "" || (*kind != "full" && *kind != "daily") {
		fatal(fmt.Errorf("builds needs -id and -kind full|daily"))
	}
	// Required, not defaulted from the id. Deriving the content day from the
	// label is the conflation this field exists to undo, and a wrong `through`
	// is worse than a missing one: it is what the publish guard compares.
	if *through == "" {
		fatal(fmt.Errorf("builds needs -through YYYYMMDD: the content day orders " +
			"the chain, and it cannot be inferred from an id that no longer means a date"))
	}
	if *kind == "daily" && *parent == "" {
		fatal(fmt.Errorf("a daily must name its -parent: the chain is explicit " +
			"because implicit chains break the first time a day is skipped"))
	}
	if *dump == "" {
		*dump = *id
	}

	var cat buildsCatalog
	if b, err := os.ReadFile(*catalog); err == nil {
		if err := json.Unmarshal(b, &cat); err != nil {
			fatal(fmt.Errorf("%s exists but does not parse; refusing to clobber it: %w",
				*catalog, err))
		}
	}

	e := buildEntry{
		ID: *id, Kind: *kind, Dump: *dump, Through: *through, Parent: *parent,
		Manifest:   "/filmstock/" + *id + "/manifest.json",
		BridgeFrom: *bridgeFrom, Bridge: *bridge, Bytes: *bytes,
	}
	if *edgesJSON != "" {
		if err := json.Unmarshal([]byte(*edgesJSON), &e.Edges); err != nil {
			fatal(fmt.Errorf("-edges is not a JSON array of routes: %w", err))
		}
	}
	if *bridgeSize >= 0 {
		e.BridgeSize = *bridgeSize
	}
	// A daily's parent must exist: the catalog is the chain, so an orphan entry
	// is a lie about applicability.
	if *parent != "" {
		found := false
		for _, x := range cat.Builds {
			if x.ID == *parent {
				found = true
				break
			}
		}
		if !found {
			fatal(fmt.Errorf("parent %s is not in the catalog", *parent))
		}
	}

	replaced := false
	for i, x := range cat.Builds {
		if x.ID == e.ID {
			cat.Builds[i] = e
			replaced = true
			break
		}
	}
	if !replaced {
		cat.Builds = append(cat.Builds, e)
	}
	orderCatalog(&cat)
	b, _ := json.MarshalIndent(cat, "", "  ")
	if err := os.WriteFile(*catalog, append(b, '\n'), 0o644); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "  %s: %d builds, latest %s (latest full %s)\n",
		*catalog, len(cat.Builds), cat.Latest, cat.LatestFull)
}

// backfillEdges gives the builds published before `edges` existed the one route
// they always had — the parent line, or the bridge on a full — priced from the
// patch files already sitting in the release root.
//
// Priced, and not left at zero, because zero is not "unknown" to a path search:
// it is "free", and a free edge wins every comparison. A catalog of unpriced
// legacy builds beside a priced full would send a fresh install walking every
// legacy patch instead of downloading the full. The sizes are on disk and
// exact, so there is no reason to guess at them.
func backfillEdges(catalogPath, root string) {
	var cat buildsCatalog
	b, err := os.ReadFile(catalogPath)
	if err != nil {
		fatal(err)
	}
	if err := json.Unmarshal(b, &cat); err != nil {
		fatal(err)
	}
	edged, sized := 0, 0
	for i, x := range cat.Builds {
		dir := filepath.Join(root, x.ID)
		if x.Kind == "full" && x.Bytes == 0 {
			dbs, _ := filepath.Glob(filepath.Join(dir, "*.db"))
			var total int64
			for _, p := range dbs {
				if fi, err := os.Stat(p); err == nil {
					total += fi.Size()
				}
			}
			if total > 0 {
				cat.Builds[i].Bytes = total
				sized++
			}
		}
		if len(x.Edges) > 0 {
			continue
		}
		from := x.Parent
		if x.Kind == "full" {
			from = x.BridgeFrom
		}
		if from == "" {
			continue // the chain's first full applies to nothing
		}
		var total int64
		for _, pat := range []string{"*.patch.sql.gz", "*.bridge.sql.gz"} {
			ps, _ := filepath.Glob(filepath.Join(dir, pat))
			for _, p := range ps {
				if fi, err := os.Stat(p); err == nil {
					total += fi.Size()
				}
			}
		}
		if total == 0 {
			fmt.Fprintf(os.Stderr, "  %s: no patch files under %s; leaving it unrouted\n", x.ID, dir)
			continue
		}
		cat.Builds[i].Edges = []patchEdge{{From: from, Bytes: total}}
		edged++
	}
	orderCatalog(&cat)
	out, _ := json.MarshalIndent(cat, "", "  ")
	if err := os.WriteFile(catalogPath, append(out, '\n'), 0o644); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "  %s: priced %d route(s) and %d full(s) of %d builds\n",
		catalogPath, edged, sized, len(cat.Builds))
}
