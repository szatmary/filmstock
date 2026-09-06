package filmstock

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/szatmary/filmstock/internal/sqldrv"
)

// Update brings dir to the newest build published behind baseURL, and returns
// the path of the core database to open.
//
// One call does the whole thing and then returns: read the catalog, work out
// whether a chain of daily patches reaches the newest build or the full has to
// be fetched whole, verify every downloaded byte against the manifest, apply
// the patches, rebuild the local full-text indexes, check the result's content
// hash, and only then record it. Nothing runs in the background and nothing
// happens on a timer; call it when you want an update.
//
//	core, build, changed, err := filmstock.Update(ctx, filmstock.DefaultBaseURL, dir)
//
// Builds land in their own directories, so an interrupted call cannot damage
// the build already in use:
//
//	<dir>/20260801/filmstock.db     the build in use
//	<dir>/20260902/…                arriving; invisible until verified
//	<dir>/state.json                which build is current
//
// The returned path is inside the NEW build's directory, so a caller reopens
// from there — including any attachments, whose files live in that same
// directory and therefore move with every build:
//
//	if changed {
//	    dir := filepath.Dir(core)
//	    db, err := filmstock.Open(core,
//	        filmstock.Attach{Schema: "text", Path: filepath.Join(dir, "filmstock-text.db")})
//	}
//
// files names the artifacts to keep current; none means the core database
// alone. baseURL is where the tree is served from — DefaultBaseURL for the
// published releases, or any directory laid out the same way (a value with no
// scheme is read as a local path).
func Update(ctx context.Context, baseURL, dir string, files ...string) (core, build string, changed bool, err error) {
	u := &updater{BaseURL: baseURL, Dir: dir, Files: files, VerifyContent: true}
	return u.update(ctx)
}

// DefaultBaseURL is where filmstock releases are published: the catalog is at
// DefaultBaseURL/builds.json and each build is a directory beside it.
//
// It is a plain constant a caller passes explicitly rather than something
// Update reaches for on its own, because which tree you update from is the one
// decision in this package that has to be visible at the call site — a test
// pointing at a fixture directory and a program pointing at the internet must
// not look the same.
//
// Nothing else published names a host: builds.json records each manifest as a
// path relative to the tree root. Moving hosts is this constant plus a bucket
// copy, and every byte already released stays valid.
const DefaultBaseURL = "https://filmstock.halide.tv"

// Held reports which build dir currently holds, "" for none.
func Held(dir string) string { return (&updater{Dir: dir}).current() }

type updater struct {
	// BaseURL serves builds.json, e.g. "https://dl.example.org/filmstock".
	// A value with no scheme is a LOCAL DIRECTORY laid out the same way —
	// "fake it until the bucket exists": point at a directory today, swap one
	// string for the real URL later, and nothing else changes.
	BaseURL string
	// Dir is where builds live locally.
	Dir string
	// Files is which of a build's artifacts to fetch. Nil means just the core
	// database; add filmstock-text.db or filmstock-vectors.db as wanted.
	Files []string
	// VerifyContent re-computes the content hash after the FTS rebuild and
	// refuses the build on mismatch. ~20 s of paranoia per update; on by
	// default via NewUpdater because a wrong database that opens cleanly is
	// worse than a slow one.
	VerifyContent bool
	// Client, for tests and proxies. Nil means a client with sane timeouts.
	Client *http.Client
	// Log, if set, reports things that were handled but are worth knowing:
	// chiefly a patch road abandoned for the full download, which is
	// recoverable and invisible without this — and which means someone is
	// paying a gigabyte for what should have been kilobytes.
	Log func(format string, args ...any)
}

func (u *updater) logf(format string, args ...any) {
	if u.Log != nil {
		u.Log(format, args...)
	}
}

func (u *updater) files() []string {
	if len(u.Files) > 0 {
		return u.Files
	}
	return []string{"filmstock.db"}
}

func (u *updater) client() *http.Client {
	if u.Client != nil {
		return u.Client
	}
	return &http.Client{Timeout: 30 * time.Minute}
}

type updaterState struct {
	Current string `json:"current"`
}

type catalogEntry struct {
	ID          string      `json:"id"`
	Kind        string      `json:"kind"`
	Parent      string      `json:"parent"`
	BridgeFrom  string      `json:"bridge_from"`
	Through     string      `json:"through"`
	Edges       []patchEdge `json:"edges"`
	Bytes       int64       `json:"bytes"`
	Epoch       int         `json:"epoch"`
	EpochReason string      `json:"epoch_reason"`
}

// patchEdge is one route into a build: apply the patches named by Suffix to
// the build named by From.
type patchEdge struct {
	From   string `json:"from"`
	Suffix string `json:"suffix"`
	Bytes  int64  `json:"bytes"`
}

// routeStep is one build's patches, applied to whatever the previous step left.
type routeStep struct {
	entry  catalogEntry
	suffix string
}
type catalog struct {
	LatestFull string         `json:"latest_full"`
	Latest     string         `json:"latest"`
	Builds     []catalogEntry `json:"builds"`
}

func (c *catalog) entry(id string) *catalogEntry {
	for i := range c.Builds {
		if c.Builds[i].ID == id {
			return &c.Builds[i]
		}
	}
	return nil
}

// newer reports whether build a holds content later than build b.
//
// By content day, never by comparing ids. An id is a label — since the
// 20260901 collision it is not even always a date — and ordering releases by
// parsing one is the mistake that ordering by `through` exists to end. An
// unknown build is not newer: refusing to move is always safe.
func (c *catalog) newer(a, b string) bool {
	if b == "" {
		return a != ""
	}
	ea, eb := c.entry(a), c.entry(b)
	if ea == nil || eb == nil {
		return false
	}
	return ea.Through > eb.Through
}

// routes returns each build's incoming edges, falling back to the single
// parent/bridge line for a catalog published before edges existed.
func (c *catalog) routes(e catalogEntry) []patchEdge {
	if len(e.Edges) > 0 {
		return e.Edges
	}
	prev := e.Parent
	if e.Kind == "full" {
		prev = e.BridgeFrom
	}
	if prev == "" {
		return nil
	}
	return []patchEdge{{From: prev}}
}

// cheapest returns the steps from cur to target that download the fewest
// bytes, oldest first, and the full to start from when starting fresh beats
// patching from what is held.
//
// The chain used to be a line, walked backwards from the target. A line is why
// a consumer six months behind had to apply every intervening daily or give up
// and refetch — a cost that grows with time away and never improves. Rollups
// make the catalog a DAG, and once more than one route exists the catalog
// should stop prescribing one: it cannot know what any given consumer holds.
//
// So this weighs every route by the bytes it actually costs, against the cost
// of simply downloading the newest full. That single rule produces the right
// answer for every case without enumerating any of them — one patch for a daily
// follower, rollups for a returning consumer, the full for a fresh install or
// for someone away so long that no path beats refetching.
//
// Returns nil steps and "" when nothing reaches the target at all.
func (c *catalog) cheapest(cur, target string) (full string, steps []routeStep, cost int64) {
	if target == "" {
		return "", nil, 0
	}
	type node struct {
		cost int64
		from string // predecessor build; "" when this node is a starting point
		step routeStep
		seed string // the full downloaded to start here, "" if continuing
	}
	// Routes never cross epochs. An epoch is bumped only to declare that
	// nothing held from before can be carried forward — a schema change old
	// patches cannot express, or a chain found to be wrong — so a consumer on
	// a retired lineage must take the full road deliberately rather than by
	// discovering that some patch will not verify. A break announced is one
	// the consumer can explain; a break inferred from a failure is
	// indistinguishable from a bug.
	tgt := c.entry(target)
	if tgt == nil {
		return "", nil, 0
	}
	era := tgt.Epoch

	best := map[string]*node{}
	if held := c.entry(cur); held != nil && held.Epoch == era {
		best[cur] = &node{cost: 0}
	}
	// Every full in this epoch is also a place to start, at the price of
	// downloading it.
	for _, e := range c.Builds {
		if e.Kind != "full" || e.Bytes <= 0 || e.Epoch != era {
			continue
		}
		if n, ok := best[e.ID]; !ok || e.Bytes < n.cost {
			best[e.ID] = &node{cost: e.Bytes, seed: e.ID}
		}
	}
	if len(best) == 0 {
		return "", nil, 0
	}

	// The graph is a few hundred nodes and edges only ever point forward in the
	// catalog's order, so one ordered sweep settles every node: a build's cost
	// is final by the time the sweep reaches it.
	for _, e := range c.Builds {
		if e.Epoch != era {
			continue
		}
		for _, edge := range c.routes(e) {
			src, ok := best[edge.From]
			if !ok {
				continue
			}
			cand := src.cost + edge.Bytes
			if n, seen := best[e.ID]; !seen || cand < n.cost {
				best[e.ID] = &node{cost: cand, from: edge.From,
					step: routeStep{entry: e, suffix: edge.Suffix}}
			}
		}
	}

	end, ok := best[target]
	if !ok {
		return "", nil, 0
	}
	for id := target; ; {
		n := best[id]
		if n.seed != "" || n.from == "" {
			full = n.seed
			break
		}
		steps = append(steps, n.step)
		id = n.from
	}
	for i, j := 0, len(steps)-1; i < j; i, j = i+1, j-1 {
		steps[i], steps[j] = steps[j], steps[i]
	}
	return full, steps, end.cost
}

type buildManifest struct {
	Dump string `json:"dump"`
	// The content-hash rules this build was published under. A consumer whose
	// own rules are older cannot verify it — not the patch road and not the
	// full — so this has to be read before any work, not discovered after.
	ContentHashV int `json:"content_hash_version"`
	Files        map[string]struct {
		Size    int64  `json:"size"`
		SHA256  string `json:"sha256"`
		Content string `json:"content_hash"`
	} `json:"files"`
}

// Current reports the build this directory holds, "" for none.
func (u *updater) current() string {
	b, err := os.ReadFile(filepath.Join(u.Dir, "state.json"))
	if err != nil {
		return ""
	}
	var s updaterState
	if json.Unmarshal(b, &s) != nil {
		return ""
	}
	return s.Current
}

// Check reports the newest build available, and whether it is newer than
// what is held.
func (u *updater) check(ctx context.Context) (latest string, newer bool, err error) {
	cat, err := u.catalog(ctx)
	if err != nil {
		return "", false, err
	}
	return cat.Latest, cat.Latest > u.current(), nil
}

func (u *updater) catalog(ctx context.Context) (*catalog, error) {
	var cat catalog
	if err := u.getJSON(ctx, u.BaseURL+"/builds.json", &cat); err != nil {
		return nil, err
	}
	if cat.Latest == "" && cat.LatestFull != "" {
		cat.Latest = cat.LatestFull
	}
	if cat.Latest == "" {
		return nil, fmt.Errorf("filmstock: catalog lists no builds")
	}
	return &cat, nil
}

// Update brings the directory to the newest build if one is newer than what
// is held.
//
// It returns the path of the core database in the NEW build's directory. A
// caller reopens from there — including any attachments, whose files live in
// that same directory and therefore move with every build:
//
//	core, build, changed, err := u.Update(ctx)
//	if changed {
//	    dir := filepath.Dir(core)
//	    fresh, err := filmstock.Open(core,
//	        filmstock.Attach{Schema: "text", Path: filepath.Join(dir, "filmstock-text.db")})
//	    // start serving from fresh, then close the old handle
//	}
//
// Reopening is the caller's to do because only the caller knows which
// databases it attached and when its in-flight work is finished with the old
// handle — via the patch chain when the catalog offers one, downloading the
// build whole when it does not or when the patched result fails to verify.
// It returns the path of the ready-to-open core database and the build id;
// changed is false when there was nothing to do.
func (u *updater) update(ctx context.Context) (corePath, build string, changed bool, err error) {
	cat, err := u.catalog(ctx)
	if err != nil {
		return "", "", false, err
	}
	latest, cur := cat.Latest, u.current()
	// Stand pat only when the catalog positively says there is nothing newer.
	// A held build the catalog no longer lists — pruned, or from a chain that
	// has been superseded — is not grounds for refusing to move: it just means
	// the search below has to start from a full instead of from here.
	if cur != "" && cur == latest {
		return filepath.Join(u.Dir, cur, "filmstock.db"), cur, false, nil
	}
	if held := cat.entry(cur); held != nil && !cat.newer(latest, cur) {
		return filepath.Join(u.Dir, cur, "filmstock.db"), cur, false, nil
	}

	// What the catalog says is cheapest from here: possibly a full to seed
	// from, then the patches to ride. The rule is bytes, and it covers a fresh
	// install, a daily follower and a consumer years behind without any of
	// them being special-cased.
	if held := cat.entry(cur); held != nil {
		if tip := cat.entry(latest); tip != nil && tip.Epoch != held.Epoch {
			why := ""
			for _, e := range cat.Builds {
				if e.Epoch == tip.Epoch && e.EpochReason != "" {
					why = e.EpochReason
					break
				}
			}
			u.logf("filmstock: %s is from epoch %d, which has been retired (now %d: %s); "+
				"nothing held can be carried forward, taking the full road",
				cur, held.Epoch, tip.Epoch, why)
		}
	}
	// Can this client verify that build at all?
	//
	// Content hashes are versioned, and the version is part of the hashed
	// bytes, so a client with older rules mismatches every build published
	// under newer ones. Both roads then fail: the patch chain refuses, and the
	// full road downloads ~1.3 GB, rebuilds its indexes, re-hashes, and refuses
	// that too — reporting a content-hash mismatch, which reads like a corrupt
	// build rather than an out-of-date client. It fails safe and it never
	// moves, and it repeats the work every run.
	//
	// The manifest says which rules it used, so ask first. Refusing here costs
	// one small GET and names the actual problem.
	var tipMan buildManifest
	if err := u.getJSON(ctx, u.BaseURL+"/"+latest+"/manifest.json", &tipMan); err != nil {
		return "", "", false, err
	}
	if tipMan.ContentHashV > ContentHashVersion {
		return "", "", false, fmt.Errorf(
			"filmstock: build %s was published with content-hash rules v%d and this "+
				"client understands v%d; it cannot verify that build by any road. "+
				"Staying on %s — upgrade filmstock to move",
			latest, tipMan.ContentHashV, ContentHashVersion, cur)
	}

	seed, steps, cost := cat.cheapest(cur, latest)
	if seed != "" || len(steps) > 0 {
		base := cur
		if seed != "" {
			u.logf("filmstock: %s -> %s via full %s + %d patch step(s), %d bytes",
				cur, latest, seed, len(steps), cost)
			if err := u.fetchFull(ctx, seed); err != nil {
				return "", "", false, err
			}
			base = seed
		} else {
			u.logf("filmstock: %s -> %s via %d patch step(s), %d bytes", cur, latest, len(steps), cost)
		}
		if len(steps) == 0 {
			return u.finishBuild(ctx, base)
		}
		if err := u.applyChain(ctx, base, steps); err == nil {
			return u.finishBuild(ctx, latest)
		} else {
			// A patch that lies is not papered over by fetching the build
			// whole: the full road below lands honestly on a full instead.
			u.logf("filmstock: patch road %s -> %s failed, falling back to the full: %v", base, latest, err)
			os.RemoveAll(filepath.Join(u.Dir, latest))
		}
	}

	// The full road. Only fulls host their databases, so this lands on the
	// newest full and rides whatever patches reach the tip from there. When
	// nothing past the full is reachable, the full itself is the honest
	// destination — newer than what is held, and the state says what it is.
	// A full no newer than what is held is no destination at all: never
	// downgrade.
	full := cat.LatestFull
	if full == "" {
		return "", "", false, fmt.Errorf("filmstock: catalog lists no full build")
	}
	// As above: only a held build the catalog still lists can establish that
	// the newest full would be a downgrade.
	if held := cat.entry(cur); held != nil && !cat.newer(full, cur) {
		return "", "", false, fmt.Errorf(
			"filmstock: no patch road from %s to %s, and the newest full %s is not newer", cur, latest, full)
	}
	if err := u.fetchFull(ctx, full); err != nil {
		return "", "", false, err
	}
	if full == latest {
		return u.finishBuild(ctx, full)
	}
	if _, steps, _ := cat.cheapest(full, latest); len(steps) > 0 {
		if err := u.applyChain(ctx, full, steps); err == nil {
			return u.finishBuild(ctx, latest)
		} else {
			u.logf("filmstock: patch road %s -> %s failed, staying on the full: %v", full, latest, err)
			os.RemoveAll(filepath.Join(u.Dir, latest))
		}
	}
	return u.finishBuild(ctx, full)
}

// fetchFull downloads a full build's files whole, verifying each.
func (u *updater) fetchFull(ctx context.Context, id string) error {
	var man buildManifest
	if err := u.getJSON(ctx, u.BaseURL+"/"+id+"/manifest.json", &man); err != nil {
		return err
	}
	dir := filepath.Join(u.Dir, id)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	for _, name := range u.files() {
		want, ok := man.Files[name]
		if !ok {
			return fmt.Errorf("filmstock: build %s has no %s", id, name)
		}
		dst := filepath.Join(dir, name)
		// Already present and verified — an interrupted run resumes for free.
		if sum, err := fileSHA256(dst); err == nil && sum == want.SHA256 {
			continue
		}
		if err := u.fetch(ctx, u.BaseURL+"/"+id+"/"+name, dst, want.SHA256); err != nil {
			return err
		}
	}
	return nil
}

// finishBuild rebuilds the local FTS, optionally re-verifies content, and
// flips state.json — the commit point; everything before it is invisible.
func (u *updater) finishBuild(ctx context.Context, latest string) (string, string, bool, error) {
	var man buildManifest
	if err := u.getJSON(ctx, u.BaseURL+"/"+latest+"/manifest.json", &man); err != nil {
		return "", "", false, err
	}
	dir := filepath.Join(u.Dir, latest)
	core := filepath.Join(dir, "filmstock.db")
	h, err := sql.Open(sqldrv.Name, sqldrv.DSN(core, false))
	if err != nil {
		return "", "", false, err
	}
	if err := RebuildFTS(h); err != nil {
		h.Close()
		return "", "", false, err
	}
	// A compressed database grows while it is written: the rebuilt FTS lands
	// as new extents and the space the old ones held is not reused until
	// something compacts it. Left alone the core came out at 686 MB — larger
	// than the plain file it replaced — against 215 MB after a VACUUM. Plain
	// files are left alone: SQLite's own free-list already reuses that space,
	// and a VACUUM would cost a full rewrite for nothing.
	if compressed, err := isCompressed(core); err != nil {
		h.Close()
		return "", "", false, err
	} else if compressed {
		// Twice, deliberately. A container cannot reclaim space its own
		// in-flight commit freed — pending extents are released only once
		// that commit's header flip is durable — so the first VACUUM after a
		// bulk FTS rebuild rewrites the database while reclaiming almost
		// nothing, and the second collects what the first freed. Measured on
		// the 20260901 build: 1.03 GB after one pass, 370 MB after two, and
		// a third changes nothing.
		for range 2 {
			if _, err := h.Exec(`VACUUM`); err != nil {
				h.Close()
				return "", "", false, fmt.Errorf("filmstock: compacting %s: %w", core, err)
			}
		}
	}
	if u.VerifyContent {
		// Under the rules the BUILD declares, not the client's current ones: a
		// published manifest's number was produced once, by the version in
		// force that day, and that is the only version it reproduces under.
		got, _, err := ContentHashAt(h, man.ContentHashV)
		if err != nil {
			h.Close()
			return "", "", false, err
		}
		if want := man.Files["filmstock.db"].Content; want != "" && got != want {
			h.Close()
			return "", "", false, fmt.Errorf(
				"filmstock: build %s content hash mismatch after rebuild: got %s want %s",
				latest, got, want)
		}
	}
	if err := h.Close(); err != nil {
		return "", "", false, err
	}
	sb, _ := json.Marshal(updaterState{Current: latest})
	if err := os.WriteFile(filepath.Join(u.Dir, "state.json"), sb, 0o644); err != nil {
		return "", "", false, err
	}
	return core, latest, true, nil
}

// applyChain copies the current build's files into the target's directory and
// applies each step's patches in order, then demands every file's content
// hash equal the target manifest's. Every failure is an error; the caller
// falls back to the whole download.
func (u *updater) applyChain(ctx context.Context, cur string, steps []routeStep) error {
	target := steps[len(steps)-1].entry.ID
	dir := filepath.Join(u.Dir, target)
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	for _, name := range u.files() {
		if err := copyLocal(filepath.Join(u.Dir, cur, name), filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("filmstock: carrying %s forward from %s: %w", name, cur, err)
		}
	}
	for _, st := range steps {
		step := st.entry
		var man buildManifest
		if err := u.getJSON(ctx, u.BaseURL+"/"+step.ID+"/manifest.json", &man); err != nil {
			return err
		}
		// A rollup's files carry the tag naming where they start from, so one
		// build directory can host several routes in without collision.
		suffix := st.suffix + ".patch.sql.gz"
		if step.Kind == "full" && st.suffix == "" {
			suffix = ".bridge.sql.gz"
		}
		for _, name := range u.files() {
			want, ok := man.Files[name+suffix]
			if !ok {
				// No patch for this file in this step. A full hosts its
				// databases, so take the file whole; a daily does not, and a
				// file that debuts mid-chain is unreachable until the next
				// full — which is where new files are supposed to debut.
				whole, ok := man.Files[name]
				if !ok {
					return fmt.Errorf("filmstock: build %s has neither %s nor its patch", step.ID, name)
				}
				if err := u.fetch(ctx, u.BaseURL+"/"+step.ID+"/"+name,
					filepath.Join(dir, name), whole.SHA256); err != nil {
					return err
				}
				continue
			}
			raw, err := u.getBlob(ctx, u.BaseURL+"/"+step.ID+"/"+name+suffix, want.SHA256)
			if err != nil {
				return err
			}
			gz, err := gzip.NewReader(bytes.NewReader(raw))
			if err != nil {
				return err
			}
			sql, err := io.ReadAll(gz)
			if err != nil {
				return err
			}
			if err := applySQL(filepath.Join(dir, name), sql); err != nil {
				return fmt.Errorf("filmstock: applying %s of %s: %w", name+suffix, step.ID, err)
			}
		}
	}
	// The proof: the patched files must BE the target build, content-wise.
	var man buildManifest
	if err := u.getJSON(ctx, u.BaseURL+"/"+target+"/manifest.json", &man); err != nil {
		return err
	}
	for _, name := range u.files() {
		want := man.Files[name].Content
		if want == "" {
			continue
		}
		h, err := sql.Open(sqldrv.Name, sqldrv.DSN(filepath.Join(dir, name), true))
		if err != nil {
			return fmt.Errorf("filmstock: opening patched %s: %w", name, err)
		}
		got, _, err := ContentHashAt(h, man.ContentHashV)
		h.Close()
		if err != nil {
			return fmt.Errorf("filmstock: hashing patched %s: %w", name, err)
		}
		if got != want {
			return fmt.Errorf("filmstock: %s after patch chain: content %s, want %s", name, got, want)
		}
	}
	return nil
}

// applySQL runs a patch against one database file.
//
// The statements go in batches rather than as one transaction because that
// is both faster and denser against a compressed container: the same 16,822
// statement patch applied to the same 286 MB container took 4.8 s and left
// 311 MB in batches, against 41.6 s and 359 MB as one transaction. A bulk
// transaction scatters free gaps the container's online compaction can only
// walk back gradually, so a long single commit ends up carrying its own
// fragmentation; committing in steps lets each one converge.
//
// (Batching also began as the workaround for an upstream bug where a large
// transaction against a container failed with a false SQLITE_FULL. That is
// fixed — internal/zstdvfs is pinned past it — and single transactions now
// succeed; batching stays on the numbers above, not on the bug.)
//
// Batching gives up all-or-nothing at the FILE level, which this caller does
// not need: patches are applied inside a staging build directory that is
// discarded whole if anything fails, so the build — not the file — is the
// unit that commits.
func applySQL(path string, patch []byte) error {
	if len(patch) == 0 {
		return nil
	}
	h, err := sql.Open(sqldrv.Name, sqldrv.DSN(path, false))
	if err != nil {
		return err
	}
	defer h.Close()
	h.SetMaxOpenConns(1)
	for _, batch := range sqlBatches(patch, patchBatchStatements) {
		if _, err := h.Exec(`BEGIN IMMEDIATE`); err != nil {
			return err
		}
		if _, err := h.Exec(batch); err != nil {
			h.Exec(`ROLLBACK`)
			return err
		}
		if _, err := h.Exec(`COMMIT`); err != nil {
			return err
		}
	}
	return nil
}

// patchBatchStatements is how many statements go in one transaction: small
// enough that a container reclaims between commits rather than accumulating a
// transaction's worth of fragmentation, large enough that per-transaction
// overhead stays invisible.
const patchBatchStatements = 2000

// sqlBatches splits a patch into groups of at most n statements, cutting only
// at a semicolon that ends a statement — one inside a string literal is data.
// sqldiff emits no comments, so quoting is the only state worth tracking.
func sqlBatches(patch []byte, n int) []string {
	var out []string
	var start, count int
	inString := false
	for i := 0; i < len(patch); i++ {
		switch patch[i] {
		case '\'':
			// '' inside a literal is an escaped quote, and reads here as a
			// close immediately followed by an open — same state either way.
			inString = !inString
		case ';':
			if inString {
				continue
			}
			count++
			if count >= n {
				out = append(out, string(patch[start:i+1]))
				start, count = i+1, 0
			}
		}
	}
	if rest := strings.TrimSpace(string(patch[start:])); rest != "" {
		out = append(out, rest)
	}
	return out
}

func copyLocal(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func (u *updater) local() bool { return !strings.Contains(u.BaseURL, "://") }

func (u *updater) getJSON(ctx context.Context, url string, v any) error {
	if u.local() {
		b, err := os.ReadFile(url)
		if err != nil {
			return err
		}
		return json.Unmarshal(b, v)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	res, err := u.client().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("filmstock: GET %s: %s", url, res.Status)
	}
	return json.NewDecoder(res.Body).Decode(v)
}

// fetch streams a file to disk, hashing as it goes, and renames into place
// only when the hash matches — a torn download or a tampered byte never gets a
// real filename.
func (u *updater) fetch(ctx context.Context, url, dst, wantSHA string) error {
	var body io.ReadCloser
	if u.local() {
		f, err := os.Open(url)
		if err != nil {
			return err
		}
		body = f
	} else {
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return err
		}
		res, err := u.client().Do(req)
		if err != nil {
			return err
		}
		if res.StatusCode != 200 {
			res.Body.Close()
			return fmt.Errorf("filmstock: GET %s: %s", url, res.Status)
		}
		body = res.Body
	}
	defer body.Close()
	tmp := dst + ".partial"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	sum := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, sum), body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != wantSHA {
		os.Remove(tmp)
		return fmt.Errorf("filmstock: %s: sha256 mismatch: got %s want %s", url, got, wantSHA)
	}
	return os.Rename(tmp, dst)
}

// getBlob fetches a small file into memory, refusing a hash mismatch.
func (u *updater) getBlob(ctx context.Context, url, wantSHA string) ([]byte, error) {
	var body io.ReadCloser
	if u.local() {
		f, err := os.Open(url)
		if err != nil {
			return nil, err
		}
		body = f
	} else {
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return nil, err
		}
		res, err := u.client().Do(req)
		if err != nil {
			return nil, err
		}
		if res.StatusCode != 200 {
			res.Body.Close()
			return nil, fmt.Errorf("filmstock: GET %s: %s", url, res.Status)
		}
		body = res.Body
	}
	defer body.Close()
	b, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); wantSHA != "" && got != wantSHA {
		return nil, fmt.Errorf("filmstock: %s: sha256 mismatch: got %s want %s", url, got, wantSHA)
	}
	return b, nil
}

// isCompressed reports whether a published database is stored compressed
// rather than as a plain SQLite file. Plain files announce themselves in
// their first 16 bytes; anything else is a container, which only a cgo build
// can read.
func isCompressed(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var magic [16]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return false, err
	}
	return string(magic[:]) != "SQLite format 3\x00", nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
