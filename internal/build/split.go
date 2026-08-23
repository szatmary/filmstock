package build

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// GitHub rejects any file over 100 MB and warns above 50 MB. 90 MB leaves
// headroom for both without multiplying the part count.
const partSize = 90 << 20

const manifestName = "MANIFEST"

// CmdSplit cuts the index into git-committable parts and records a manifest.
//
// Parts are cut at fixed offsets on the RAW database rather than on a compressed
// copy. Compressing first would be ~20% smaller today, but a single changed byte
// rewrites the whole compressed stream, so every part changes on every rebuild.
// Raw parts change only where pages changed — which is worth nothing today,
// because the index is rebuilt with DROP/CREATE and no page survives, but is
// worth everything the moment the build is made deterministic. This is the
// choice that does not have to be revisited then.
func CmdSplit(args []string) {
	fs := flag.NewFlagSet("split", flag.ExitOnError)
	dbPath := fs.String("db", "out/search.db", "index to split")
	outDir := fs.String("out", "index", "directory to write parts into")
	fs.Parse(args)

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatal(err)
	}
	// Old parts must go: a smaller index would otherwise leave a stale trailing
	// part behind and join would silently produce a corrupt file.
	old, _ := filepath.Glob(filepath.Join(*outDir, "*.part*"))
	for _, f := range old {
		os.Remove(f)
	}

	in, err := os.Open(*dbPath)
	if err != nil {
		fatal(err)
	}
	defer in.Close()

	base := filepath.Base(*dbPath)
	whole := sha256.New()
	var lines []string
	buf := make([]byte, 1<<20)
	var total int64

	for n := 0; ; n++ {
		name := fmt.Sprintf("%s.part%02d", base, n)
		out, err := os.Create(filepath.Join(*outDir, name))
		if err != nil {
			fatal(err)
		}
		part := sha256.New()
		var written int64
		for written < partSize {
			want := int64(len(buf))
			if r := partSize - written; r < want {
				want = r
			}
			k, rerr := in.Read(buf[:want])
			if k > 0 {
				out.Write(buf[:k])
				part.Write(buf[:k])
				whole.Write(buf[:k])
				written += int64(k)
				total += int64(k)
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				fatal(rerr)
			}
		}
		out.Close()
		if written == 0 {
			os.Remove(filepath.Join(*outDir, name)) // exact multiple of partSize
			break
		}
		lines = append(lines, fmt.Sprintf("%s  %d  %s", hex.EncodeToString(part.Sum(nil)), written, name))
		fmt.Fprintf(os.Stderr, "  %s  %6.1f MB\n", name, float64(written)/(1<<20))
		if written < partSize {
			break
		}
	}

	man := fmt.Sprintf("# %s split into %d parts, %d bytes total\n# reassemble: filmstock join -in %s -db %s\nwhole  %s  %d  %s\n%s\n",
		base, len(lines), total, *outDir, *dbPath,
		hex.EncodeToString(whole.Sum(nil)), total, base, strings.Join(lines, "\n"))
	if err := os.WriteFile(filepath.Join(*outDir, manifestName), []byte(man), 0o644); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "%d parts, %.1f MB total -> %s\n", len(lines), float64(total)/(1<<20), *outDir)
}

// CmdJoin reassembles the index from its parts and verifies it against the
// manifest. It verifies rather than trusting: a truncated or reordered part
// produces a database that opens and returns wrong answers, which is a far worse
// failure than refusing to assemble.
func CmdJoin(args []string) {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	inDir := fs.String("in", "index", "directory holding the parts and MANIFEST")
	dbPath := fs.String("db", "out/search.db", "index to write")
	fs.Parse(args)

	raw, err := os.ReadFile(filepath.Join(*inDir, manifestName))
	if err != nil {
		fatal(fmt.Errorf("%s: %w", manifestName, err))
	}
	var wantWhole string
	var wantTotal int64
	parts := map[string]struct {
		sum  string
		size int64
	}{}
	var names []string
	for _, ln := range strings.Split(string(raw), "\n") {
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		f := strings.Fields(ln)
		if len(f) != 3 && len(f) != 4 {
			fatal(fmt.Errorf("%s: cannot parse %q", manifestName, ln))
		}
		if f[0] == "whole" {
			wantWhole = f[1]
			fmt.Sscanf(f[2], "%d", &wantTotal)
			continue
		}
		var sz int64
		fmt.Sscanf(f[1], "%d", &sz)
		parts[f[2]] = struct {
			sum  string
			size int64
		}{f[0], sz}
		names = append(names, f[2])
	}
	sort.Strings(names)

	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		fatal(err)
	}
	out, err := os.Create(*dbPath)
	if err != nil {
		fatal(err)
	}
	whole := sha256.New()
	var total int64
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(*inDir, name))
		if err != nil {
			out.Close()
			fatal(err)
		}
		want := parts[name]
		if got := hex.EncodeToString(sha256.New().Sum(nil)); got == "" && false {
			_ = got
		}
		h := sha256.Sum256(b)
		if hex.EncodeToString(h[:]) != want.sum {
			out.Close()
			fatal(fmt.Errorf("%s: checksum mismatch — the part is corrupt or not the one the manifest describes", name))
		}
		if int64(len(b)) != want.size {
			out.Close()
			fatal(fmt.Errorf("%s: got %d bytes, manifest says %d", name, len(b), want.size))
		}
		out.Write(b)
		whole.Write(b)
		total += int64(len(b))
	}
	out.Close()

	if total != wantTotal {
		fatal(fmt.Errorf("assembled %d bytes, manifest says %d", total, wantTotal))
	}
	if got := hex.EncodeToString(whole.Sum(nil)); got != wantWhole {
		fatal(fmt.Errorf("assembled index does not match the manifest checksum"))
	}
	fmt.Fprintf(os.Stderr, "%s: %d parts, %.1f MB, sha256 verified\n", *dbPath, len(names), float64(total)/(1<<20))
}
