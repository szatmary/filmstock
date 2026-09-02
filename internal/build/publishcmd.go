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

// CmdPublish turns one finished build into one release — with every patch
// APPLIED to a copy of its base and content-hash verified against the new
// build before anything is recorded. The "null op unless there is a mistake"
// promise is checked here, at publish time, not hoped for at consume time.
//
//	filmstock publish -root bucket -id 20260819 -from /var/tmp/rebuild-19
//	filmstock publish -root bucket -id 20260901 -from ... -full
//
// Only fulls host their databases; a daily is its patches. The information
// content of a chain is one full plus the patch series, so hosting a daily's
// databases would ship redundancy — but the NEXT build still needs them to
// diff against, so every build's databases live in the un-hosted work
// directory until their successor is published.
//
// With -compress a hosted full is converted to a zstdvfs container, which
// halves it (1249 MB -> 595 MB across the three files) and stays that way on
// the consumer's disk, since the VFS reads a container in place rather than
// unpacking it. The work copy stays plain — diffs run plain against plain,
// and content hashes are storage-independent, so a patch built from plain
// files verifies against a container exactly the same. It is off by default
// until a consumer can apply a daily patch to a container; see docs/TODO.md.
//
//	<root>/builds.json                   hosted: sync this tree to the bucket
//	<root>/<full>/filmstock.db …         a full's databases
//	<root>/<full>/<db>.bridge.sql.gz     previous chain tip -> this full
//	<root>/<daily>/<db>.patch.sql.gz     parent -> this daily
//	<root>/<id>/manifest.json            sizes, sha256, content hashes
//	<work>/<daily>/filmstock.db …        NOT hosted: the tip, kept for diffing
func CmdPublish(args []string) {
	fs := flag.NewFlagSet("publish", flag.ExitOnError)
	root := fs.String("root", "bucket", "release root holding builds.json and one directory per build")
	id := fs.String("id", "", "build id (YYYYMMDD, the dump it mirrors)")
	from := fs.String("from", "", "directory holding the build's *.db files")
	full := fs.Bool("full", false, "this build is a full (starts a new chain)")
	work := fs.String("work", "", "un-hosted directory holding the chain tip's databases (default <root>-work)")
	compress := fs.Bool("compress", false, "full only: host the databases as zstdvfs containers, halving them (see docs/TODO.md: VACUUM on a heavily patched container currently fails)")
	differ := fs.String("sqldiff", "./sqldiff", "the sqldiff binary (make sqldiff)")
	fs.Parse(args)
	if *id == "" || *from == "" {
		fatal(fmt.Errorf("publish needs -id YYYYMMDD and -from DIR"))
	}
	if _, err := exec.LookPath(*differ); err != nil {
		fatal(fmt.Errorf("%s: %w — build it with `make sqldiff`", *differ, err))
	}
	if *work == "" {
		*work = strings.TrimSuffix(*root, "/") + "-work"
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
	// Every build's plain databases go to the work directory as the next
	// build's diff base; a full additionally hosts compressed copies.
	dbDir := filepath.Join(*work, *id)
	if err := os.MkdirAll(dbDir, 0o777); err != nil {
		fatal(err)
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(dir)
			if dbDir != dir {
				os.RemoveAll(dbDir)
			}
		}
	}()

	// baseDBDir finds where a build's databases actually are: the work
	// directory for a daily tip, the hosted directory for a full.
	baseDBDir := func(id string) string {
		if _, err := os.Stat(filepath.Join(*work, id)); err == nil {
			return filepath.Join(*work, id)
		}
		return filepath.Join(*root, id)
	}

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
		if err := linkOrCopy(p, filepath.Join(dbDir, filepath.Base(p))); err != nil {
			fatal(err)
		}
	}
	if base != "" {
		carried, err := filepath.Glob(filepath.Join(baseDBDir(base), "*.db"))
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
			if err := linkOrCopy(p, filepath.Join(dbDir, b)); err != nil {
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
			basePath := filepath.Join(baseDBDir(base), b)
			if _, err := os.Stat(basePath); err != nil {
				fmt.Fprintf(os.Stderr, "  %s: new in this build, no patch\n", b)
				continue
			}
			newPath := filepath.Join(dbDir, b)
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

	// A full hosts its databases, compressed. The manifest then describes the
	// bytes a consumer actually downloads; the content hash is the same either
	// way, which is what lets a patch built against plain files verify against
	// a container.
	manifestFiles := map[string]string{}
	for _, b := range ordered {
		src := filepath.Join(dbDir, b)
		if !*full {
			manifestFiles[b] = src
			continue
		}
		hosted := filepath.Join(dir, b)
		if err := copyFile(src, hosted); err != nil {
			fatal(err)
		}
		if *compress {
			plain, err := os.Stat(hosted)
			if err != nil {
				fatal(err)
			}
			if err := compressDB(hosted); err != nil {
				fatal(fmt.Errorf("compressing %s: %w", b, err))
			}
			comp, err := os.Stat(hosted)
			if err != nil {
				fatal(err)
			}
			fmt.Fprintf(os.Stderr, "  %-24s compressed %5d MB -> %5d MB (%.2fx)\n",
				b, plain.Size()/1048576, comp.Size()/1048576,
				float64(plain.Size())/float64(comp.Size()))
		}
		manifestFiles[b] = hosted
	}
	patches, err := filepath.Glob(filepath.Join(dir, "*.sql.gz"))
	if err != nil {
		fatal(err)
	}
	for _, p := range patches {
		manifestFiles[filepath.Base(p)] = p
	}
	if err := writeManifest(filepath.Join(dir, "manifest.json"), *id, manifestFiles); err != nil {
		fatal(err)
	}

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

	// The old tip's databases have served their purpose: this build's patches
	// were diffed against them and verified. Only the new tip stays in work.
	tips, _ := filepath.Glob(filepath.Join(*work, "*"))
	for _, t := range tips {
		if filepath.Base(t) != *id {
			os.RemoveAll(t)
		}
	}
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

// compressDB converts a plain SQLite file into a zstdvfs container in place.
//
// The conversion is a VACUUM through the VFS — the shim writes the rebuilt
// database into container format — so it costs a full rewrite (36 s for the
// 378 MB core) and needs room for both forms while it runs. It is one-way:
// reading it back out means VACUUM INTO through a different VFS, which the
// consumer never needs, because the VFS reads containers directly.
func compressDB(path string) error {
	db, err := sql.Open(sqldrv.Name, sqldrv.DSN(path, false))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`VACUUM`); err != nil {
		return err
	}
	return db.Close()
}
