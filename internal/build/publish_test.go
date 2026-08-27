package build

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/szatmary/filmstock/internal/sqldrv"
)

func tinyBuild(t *testing.T, dir string, rows map[int]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(sqldrv.Name, filepath.Join(dir, "filmstock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE movies(id INTEGER PRIMARY KEY, title TEXT NOT NULL,
		year INTEGER, release_date TEXT, director TEXT, producer TEXT, writer TEXT,
		starring TEXT, music TEXT, distributor TEXT, country TEXT, language TEXT,
		genre TEXT, runtime TEXT, budget TEXT, gross TEXT, wikipedia_url TEXT,
		cover_image_url TEXT, cover_image_file TEXT, wiki_title TEXT)`); err != nil {
		t.Fatal(err)
	}
	for id, title := range rows {
		if _, err := db.Exec(`INSERT INTO movies(id,title,year,release_date,director,
			producer,writer,starring,music,distributor,country,language,genre,runtime,
			budget,gross,wikipedia_url,cover_image_url,cover_image_file,wiki_title)
			VALUES(?,?,1990,'','','','','','','','','','','','','','','','',?)`,
			id, title, title); err != nil {
			t.Fatal(err)
		}
	}
}

// The publisher's whole promise in one walk: a full publishes without a
// patch, a daily publishes with a verified patch and a chained catalog
// entry, and a patch that cannot reproduce its target is refused.
func TestPublishChainsAndVerifies(t *testing.T) {
	differ := "../../sqldiff"
	if _, err := exec.LookPath(differ); err != nil {
		t.Skip("no ./sqldiff binary; run `make sqldiff` to enable this test")
	}
	root := t.TempDir()
	src := t.TempDir()

	tinyBuild(t, filepath.Join(src, "a"), map[int]string{1: "Heat", 2: "Solaris"})
	CmdPublish([]string{"-root", root, "-id", "20260801",
		"-from", filepath.Join(src, "a"), "-full", "-sqldiff", differ})

	tinyBuild(t, filepath.Join(src, "b"), map[int]string{1: "Heat", 2: "Solaris", 3: "Stalker"})
	CmdPublish([]string{"-root", root, "-id", "20260802",
		"-from", filepath.Join(src, "b"), "-sqldiff", differ})

	var cat buildsCatalog
	b, err := os.ReadFile(filepath.Join(root, "builds.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &cat); err != nil {
		t.Fatal(err)
	}
	if len(cat.Builds) != 2 || cat.Builds[1].Parent != "20260801" {
		t.Fatalf("catalog = %+v; want a daily chained to the full", cat.Builds)
	}
	if cat.Latest != "20260802" || cat.LatestFull != "20260801" {
		t.Errorf("latest=%s latest_full=%s", cat.Latest, cat.LatestFull)
	}
	if _, err := os.Stat(filepath.Join(root, "20260802", "filmstock.db.patch.sql.gz")); err != nil {
		t.Errorf("daily published without its patch: %v", err)
	}
}

// A patch that produces the wrong content must never be recorded as one that
// works: the verification is the product.
func TestVerifyPatchRefusesWrongContent(t *testing.T) {
	src := t.TempDir()
	tinyBuild(t, filepath.Join(src, "a"), map[int]string{1: "Heat"})
	tinyBuild(t, filepath.Join(src, "b"), map[int]string{1: "Heat", 2: "Solaris"})
	base := filepath.Join(src, "a", "filmstock.db")
	want := filepath.Join(src, "b", "filmstock.db")

	// A patch that changes nothing cannot reproduce b from a.
	if err := verifyPatch(base, nil, want); err == nil {
		t.Fatal("an empty patch verified against a different target")
	}
	// The real change does.
	right := `INSERT INTO movies(id,title,year,release_date,director,producer,writer,
		starring,music,distributor,country,language,genre,runtime,budget,gross,
		wikipedia_url,cover_image_url,cover_image_file,wiki_title)
		VALUES(2,'Solaris',1990,'','','','','','','','','','','','','','','','','Solaris');`
	if err := verifyPatch(base, []byte(right), want); err != nil {
		t.Fatalf("the correct patch failed verification: %v", err)
	}
}
