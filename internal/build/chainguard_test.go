package build

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The regression this file exists for.
//
// On 2026-09-05 a full rebuilt from the 20260901 dump was a candidate for
// publication onto a chain whose tip already held 20260902. Every check the
// publisher had would have passed: the bridge generates, applies to a copy of
// its base, and reproduces the build's content hashes exactly. It is a correct
// patch to the wrong target — it walks consumers backwards, and nothing would
// have noticed until the next full a month later.
func TestPublishRefusesToBridgeBackwards(t *testing.T) {
	cat := buildsCatalog{
		Latest: "20260902",
		Builds: []buildEntry{
			{ID: "20260801", Kind: "full", Through: "20260801"},
			{ID: "20260902", Kind: "daily", Through: "20260902"},
		},
	}

	err := guardThrough(cat, "20260902", "20260901")
	if err == nil {
		t.Fatal("published a build whose content stops before the tip's")
	}
	// The message has to say what to do about it: this fires unattended, and
	// the operator reading it is a cron mail.
	for _, want := range []string{"20260901", "20260902", "catchup"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}

	// Equal is the case worth aiming for — same day by two routes, so the
	// bridge is pure drift — and must be allowed.
	if err := guardThrough(cat, "20260902", "20260902"); err != nil {
		t.Errorf("refused a same-day bridge: %v", err)
	}
	// Ahead is allowed too: the rebuild replayed the tip's deltas and more, so
	// it is still a superset.
	if err := guardThrough(cat, "20260902", "20260905"); err != nil {
		t.Errorf("refused a build ahead of the tip: %v", err)
	}
}

// The content day cannot be inferred from the id, because the id no longer
// means a date. A missing `through` is a refusal, never a guess.
func TestPublishRequiresTheContentDay(t *testing.T) {
	cat := buildsCatalog{Latest: "20260902",
		Builds: []buildEntry{{ID: "20260902", Kind: "daily", Through: "20260902"}}}
	if err := guardThrough(cat, "20260902", ""); err == nil {
		t.Fatal("published without a content day")
	}
	// A tip that predates the field is a refusal with the migration named,
	// not a silent comparison against an empty string — which would compare
	// less than every real day and admit anything.
	stale := buildsCatalog{Latest: "20260902",
		Builds: []buildEntry{{ID: "20260902", Kind: "daily"}}}
	err := guardThrough(stale, "20260902", "20260905")
	if err == nil {
		t.Fatal("compared against a tip that states no through")
	}
	if !strings.Contains(err.Error(), "backfill-through") {
		t.Errorf("refusal does not name the migration: %v", err)
	}
}

// The chain is ordered by content day, not by parsing the id as a date. An
// opaque id must not disturb the order, which is the whole point of making
// ids opaque.
func TestCatalogOrdersByContentDay(t *testing.T) {
	cat := buildsCatalog{Builds: []buildEntry{
		{ID: "r0003", Kind: "daily", Through: "20260905"},
		{ID: "20260801", Kind: "full", Through: "20260801"},
		{ID: "r0002", Kind: "full", Through: "20260904"},
		{ID: "20260902", Kind: "daily", Through: "20260902"},
	}}
	orderCatalog(&cat)

	var got []string
	for _, b := range cat.Builds {
		got = append(got, b.ID)
	}
	want := []string{"20260801", "20260902", "r0002", "r0003"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v; want %v", got, want)
		}
	}
	if cat.Latest != "r0003" {
		t.Errorf("latest = %s; want the highest content day", cat.Latest)
	}
	if cat.LatestFull != "r0002" {
		t.Errorf("latest_full = %s; want the newest full by content day", cat.LatestFull)
	}
}

// Two builds may legally reach the same content day — a full and the daily it
// supersedes — so the order must stay total and reproducible.
func TestCatalogOrderIsStableOnEqualDays(t *testing.T) {
	mk := func() buildsCatalog {
		return buildsCatalog{Builds: []buildEntry{
			{ID: "r0002", Kind: "full", Through: "20260904"},
			{ID: "20260904", Kind: "daily", Through: "20260904"},
		}}
	}
	a, b := mk(), mk()
	b.Builds[0], b.Builds[1] = b.Builds[1], b.Builds[0]
	orderCatalog(&a)
	orderCatalog(&b)
	if a.Builds[0].ID != b.Builds[0].ID || a.Latest != b.Latest {
		t.Fatalf("order depends on insertion sequence: %s/%s vs %s/%s",
			a.Builds[0].ID, a.Latest, b.Builds[0].ID, b.Latest)
	}
}

// The migration is an assertion about what those ids already meant, made once.
func TestBackfillThrough(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "builds.json")
	old := `{"latest":"20260902","builds":[
	  {"id":"20260801","kind":"full","dump":"20260801","manifest":"/f/20260801/manifest.json"},
	  {"id":"20260902","kind":"daily","dump":"20260902","parent":"20260801","manifest":"/f/20260902/manifest.json"}]}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	backfillThrough(path)

	var cat buildsCatalog
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &cat); err != nil {
		t.Fatal(err)
	}
	for _, e := range cat.Builds {
		if e.Through != e.ID {
			t.Errorf("%s: through = %q; want the id it already meant", e.ID, e.Through)
		}
	}
	if cat.Latest != "20260902" {
		t.Errorf("latest = %s after backfill", cat.Latest)
	}
	// And the backfilled tip must now satisfy the guard it exists to feed.
	if err := guardThrough(cat, "20260902", "20260905"); err != nil {
		t.Errorf("backfilled catalog still fails the guard: %v", err)
	}
}
