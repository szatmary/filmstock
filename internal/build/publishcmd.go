package build

import (
	"bufio"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/szatmary/filmstock"
	"github.com/szatmary/filmstock/internal/sqldrv"
)

// CmdPublish turns one finished build into one release: files copied into the
// per-build directory, a patch from the parent for every database, the bridge
// when a full supersedes an existing chain, the manifest, and the catalog
// entry — with every patch APPLIED to a copy of its parent and content-hash
// verified against the new build before anything is recorded. The "null op
// unless there is a mistake" promise is checked here, at publish time, not
// hoped for at consume time.
//
//	filmstock publish -root bucket -id 20260819 -from /var/tmp/rebuild-19
//	filmstock publish -root bucket -id 20260901 -from ... -full
//
// Layout written under -root, matching what builds.json states:
//
//	<root>/builds.json
//	<root>/<id>/filmstock.db …           the build's databases
//	<root>/<id>/<db>.patch.sql.gz        daily: parent -> this build
//	<root>/<id>/<db>.bridge.sql.gz       full: previous chain tip -> this build
//	<root>/<id>/manifest.json            sizes, sha256, content hashes
func CmdPublish(args []string) {
	fs := flag.NewFlagSet("publish", flag.ExitOnError)
	root := fs.String("root", "bucket", "release root holding builds.json and one directory per build")
	id := fs.String("id", "", "build id (YYYYMMDD, the dump it mirrors)")
	from := fs.String("from", "", "directory holding the build's *.db files")
	full := fs.Bool("full", false, "this build is a full (starts a new chain)")
	differ := fs.String("sqldiff", "./sqldiff", "the sqldiff binary (make sqldiff)")
	fs.Parse(args)
	if *id == "" || *from == "" {
		fatal(fmt.Errorf("publish needs -id YYYYMMDD and -from DIR"))
	}
	if _, err := exec.LookPath(*differ); err != nil {
		fatal(fmt.Errorf("%s: %w — build it with `make sqldiff`", *differ, err))
	}

	cat := readCatalog(filepath.Join(*root, "builds.json"))

	// What this build diffs against: a daily patches its parent (the chain
	// tip); a full bridges from the chain it supersedes. Both are the current
	// latest — the difference is only what the diff is called and promises.
	base := cat.Latest
	kind := "daily"
	if *full {
		kind = "full"
	} else if base == "" {
		fatal(fmt.Errorf("the catalog is empty; the first build must be -full"))
	}

	dir := filepath.Join(*root, *id)
	if _, err := os.Stat(dir); err == nil {
		fatal(fmt.Errorf("%s already exists; a published build is immutable", dir))
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		fatal(err)
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(dir)
		}
	}()

	// The build's databases. A file the build did not produce is carried
	// forward from the base unchanged — a daily that did not re-embed vectors
	// still publishes a complete build.
	names, err := filepath.Glob(filepath.Join(*from, "*.db"))
	if err != nil {
		fatal(err)
	}
	files := map[string]bool{}
	for _, p := range names {
		files[filepath.Base(p)] = true
		if err := linkOrCopy(p, filepath.Join(dir, filepath.Base(p))); err != nil {
			fatal(err)
		}
	}
	if base != "" {
		carried, err := filepath.Glob(filepath.Join(*root, base, "*.db"))
		if err != nil {
			fatal(err)
		}
		for _, p := range carried {
			b := filepath.Base(p)
			if files[b] {
				continue
			}
			fmt.Fprintf(os.Stderr, "  %s: not in %s; carried forward from %s\n", b, *from, base)
			files[b] = true
			if err := linkOrCopy(p, filepath.Join(dir, b)); err != nil {
				fatal(err)
			}
		}
	}
	if len(files) == 0 {
		fatal(fmt.Errorf("%s holds no *.db files", *from))
	}
	ordered := make([]string, 0, len(files))
	for b := range files {
		ordered = append(ordered, b)
	}
	sort.Strings(ordered)

	// Patch, then prove the patch: apply it to a copy of the base and demand
	// the copy's content hash equal the new build's. A patch that cannot
	// reproduce its target is not published.
	suffix := ".patch.sql"
	if *full {
		suffix = ".bridge.sql"
	}
	totalStatements := 0
	if base != "" {
		for _, b := range ordered {
			basePath := filepath.Join(*root, base, b)
			if _, err := os.Stat(basePath); err != nil {
				fmt.Fprintf(os.Stderr, "  %s: new in this build, no patch\n", b)
				continue
			}
			newPath := filepath.Join(dir, b)
			patchPath := filepath.Join(dir, b+suffix+".gz")
			n, raw, err := writeDiff(*differ, basePath, newPath, patchPath)
			if err != nil {
				fatal(err)
			}
			totalStatements += n
			if err := verifyPatch(basePath, raw, newPath); err != nil {
				fatal(fmt.Errorf("%s: the patch does not reproduce the build: %w", b, err))
			}
			fi, _ := os.Stat(patchPath)
			fmt.Fprintf(os.Stderr, "  %-24s %s %8d statements  %9d bytes gz  verified\n",
				b, suffix[1:], n, fi.Size())
		}
	}

	CmdManifest([]string{"-dir", dir, "-dump", *id})

	bargs := []string{"-catalog", filepath.Join(*root, "builds.json"),
		"-id", *id, "-kind", kind}
	if kind == "daily" {
		bargs = append(bargs, "-parent", base)
	} else if base != "" {
		bargs = append(bargs,
			"-bridge-from", base,
			"-bridge", "/filmstock/"+*id+"/filmstock.db"+suffix+".gz",
			"-bridge-statements", fmt.Sprint(totalStatements))
	}
	CmdBuilds(bargs)
	ok = true
}

func readCatalog(path string) buildsCatalog {
	var cat buildsCatalog
	b, err := os.ReadFile(path)
	if err != nil {
		return cat
	}
	if err := json.Unmarshal(b, &cat); err != nil {
		fatal(fmt.Errorf("%s exists but does not parse; refusing to publish against it: %w", path, err))
	}
	return cat
}

// writeDiff runs sqldiff base -> next into a gzipped patch file, returning the
// approximate statement count (terminator lines; string literals holding a
// bare ";" line overcount, which is fine for a meter) and the raw SQL for the
// verification step.
func writeDiff(differ, basePath, newPath, outPath string) (int, []byte, error) {
	cmd := exec.Command(differ, basePath, newPath)
	var errb strings.Builder
	cmd.Stderr = &errb
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, nil, err
	}
	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}
	raw, err := io.ReadAll(stdout)
	if err != nil {
		cmd.Wait()
		return 0, nil, err
	}
	if err := cmd.Wait(); err != nil {
		return 0, nil, fmt.Errorf("sqldiff %s %s: %w\n%s", basePath, newPath, err, errb.String())
	}
	f, err := os.Create(outPath)
	if err != nil {
		return 0, nil, err
	}
	gz := gzip.NewWriter(f)
	if _, err := gz.Write(raw); err != nil {
		f.Close()
		return 0, nil, err
	}
	if err := gz.Close(); err != nil {
		f.Close()
		return 0, nil, err
	}
	if err := f.Close(); err != nil {
		return 0, nil, err
	}
	n := 0
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		if strings.HasSuffix(strings.TrimSpace(sc.Text()), ";") {
			n++
		}
	}
	return n, raw, nil
}

// verifyPatch applies raw SQL to a scratch copy of basePath and compares
// content hashes with wantPath.
func verifyPatch(basePath string, raw []byte, wantPath string) error {
	tmp := wantPath + ".verify"
	defer os.Remove(tmp)
	if err := copyFile(basePath, tmp); err != nil {
		return err
	}
	db, err := sql.Open(sqldrv.Name, tmp)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{`PRAGMA journal_mode=OFF`, `PRAGMA synchronous=OFF`, `BEGIN`} {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			return err
		}
	}
	if len(raw) > 0 {
		if _, err := db.Exec(string(raw)); err != nil {
			db.Close()
			return fmt.Errorf("applying: %w", err)
		}
	}
	if _, err := db.Exec(`COMMIT`); err != nil {
		db.Close()
		return err
	}
	got, _, err := filmstock.ContentHash(db)
	db.Close()
	if err != nil {
		return err
	}
	want, _, err := contentHashOf(wantPath)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("content hash %s after patch, want %s", got, want)
	}
	return nil
}

func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return copyFile(src, dst)
}

func copyFile(src, dst string) error {
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
