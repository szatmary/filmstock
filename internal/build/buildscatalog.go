package build

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
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
// The bridge_size on a full is the pipeline's public integrity meter: it is
// the statement count of the diff between [previous full + every daily] and
// [this full, rebuilt from scratch]. Its baseline is small but not zero —
// page deletions only arrive via fulls, dump boundaries fuzz by hours, and
// the Wikidata refresh moves records no enwiki edit touched. A spike is a
// bug, published where everyone can see it.
type buildsCatalog struct {
	Updated    string       `json:"updated"`
	LatestFull string       `json:"latest_full,omitempty"`
	Latest     string       `json:"latest,omitempty"`
	Builds     []buildEntry `json:"builds"`
}

type buildEntry struct {
	ID       string `json:"id"`               // e.g. 20260801, 20260802
	Kind     string `json:"kind"`             // full | daily
	Dump     string `json:"dump"`             // the Wikimedia dump it mirrors, 1:1
	Parent   string `json:"parent,omitempty"` // the build this applies on top of
	Manifest string `json:"manifest"`         // path to the build's manifest
	// Full builds only: the reconciliation from the previous chain.
	BridgeFrom string `json:"bridge_from,omitempty"` // last daily of the old chain
	Bridge     string `json:"bridge,omitempty"`      // path to the bridge patch
	BridgeSize int    `json:"bridge_statements,omitempty"`
}

// CmdBuilds adds or updates one entry in the catalog.
func CmdBuilds(args []string) {
	fs := flag.NewFlagSet("builds", flag.ExitOnError)
	catalog := fs.String("catalog", "publish/builds.json", "the catalog to maintain")
	id := fs.String("id", "", "build id (YYYYMMDD)")
	kind := fs.String("kind", "", "full or daily")
	dump := fs.String("dump", "", "the Wikimedia dump this mirrors (default: the id)")
	parent := fs.String("parent", "", "build this applies on top of (required for daily)")
	bridgeFrom := fs.String("bridge-from", "", "full only: last daily of the previous chain")
	bridge := fs.String("bridge", "", "full only: path to the bridge patch")
	bridgeSize := fs.Int("bridge-statements", -1, "full only: statement count of the bridge")
	fs.Parse(args)
	if *id == "" || (*kind != "full" && *kind != "daily") {
		fatal(fmt.Errorf("builds needs -id YYYYMMDD and -kind full|daily"))
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
		ID: *id, Kind: *kind, Dump: *dump, Parent: *parent,
		Manifest:   "/filmstock/" + *id + "/manifest.json",
		BridgeFrom: *bridgeFrom, Bridge: *bridge,
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
	sort.Slice(cat.Builds, func(i, j int) bool { return cat.Builds[i].ID < cat.Builds[j].ID })

	cat.Updated = time.Now().UTC().Format(time.RFC3339)
	cat.Latest, cat.LatestFull = "", ""
	for _, x := range cat.Builds {
		cat.Latest = x.ID
		if x.Kind == "full" {
			cat.LatestFull = x.ID
		}
	}
	b, _ := json.MarshalIndent(cat, "", "  ")
	if err := os.WriteFile(*catalog, append(b, '\n'), 0o644); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "  %s: %d builds, latest %s (latest full %s)\n",
		*catalog, len(cat.Builds), cat.Latest, cat.LatestFull)
}
