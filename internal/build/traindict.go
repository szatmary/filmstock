package build

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/szatmary/filmstock"
)

// CmdTrainDict rebuilds the per-kind compression dictionaries from a real store.
//
// This is expected to be run repeatedly, not once. A dictionary is only as good
// as the records it saw, and the corpus changes with every dump — so the
// dictionaries checked into dict/ are a starting point, not a fixed asset.
//
// It shells out to `zstd --train`. Building a dictionary is a maintainer's job,
// not a consumer's, so an external tool here costs nothing at the point of use —
// the same way the Makefile already wants lbzip2 to read the dumps.
//
// Changing a dictionary invalidates the store that was built with it: the
// dictionary's identity is recorded in the store header and a mismatch is
// refused at Open. So training is always followed by a rebuild, which is why
// this writes the dictionaries and stops rather than trying to migrate anything.
func CmdTrainDict(args []string) {
	fs := flag.NewFlagSet("train-dict", flag.ExitOnError)
	records := fs.String("records", "filmstock-data", "record store to train from")
	out := fs.String("out", "dict", "directory to write <kind>.dict into")
	size := fs.Int("size", 32768, "dictionary size in bytes (zlib uses at most 32 KiB)")
	sample := fs.Int("sample", 20000, "records per kind to train on")
	fs.Parse(args)

	if _, err := exec.LookPath("zstd"); err != nil {
		fatal(fmt.Errorf("train-dict needs zstd on PATH (`apt install zstd`): %w", err))
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal(err)
	}

	for _, kind := range []string{
		filmstock.KindMovie, filmstock.KindTelevision,
		filmstock.KindPerson, filmstock.KindEvent,
	} {
		tmp, err := os.MkdirTemp("", "filmstock-train-"+kind)
		if err != nil {
			fatal(err)
		}
		n, total := 0, 0
		err = filmstock.WalkStore(*records, kind, func(r filmstock.StoredRecord) error {
			if n >= *sample {
				return nil
			}
			total += len(r.Data)
			p := filepath.Join(tmp, fmt.Sprintf("%d.json", n))
			n++
			return os.WriteFile(p, r.Data, 0o644)
		})
		if err != nil {
			os.RemoveAll(tmp)
			fmt.Fprintf(os.Stderr, "  %-12s skipped: %v\n", kind, err)
			continue
		}
		// zstd's own guidance: a dictionary wants at least 10x its size in
		// source, preferably 100x. Say so rather than shipping a dictionary
		// overfitted to a handful of records, which is exactly how the first
		// people dictionary went wrong.
		if total < *size*10 {
			fmt.Fprintf(os.Stderr, "  %-12s WARNING: only %d bytes across %d records, "+
				"under 10x the %d-byte dictionary — result will be poor\n",
				kind, total, n, *size)
		}
		if n == 0 {
			os.RemoveAll(tmp)
			fmt.Fprintf(os.Stderr, "  %-12s no records\n", kind)
			continue
		}
		dst := filepath.Join(*out, kind+".dict")
		// The file list goes via a response file: a kind can hold hundreds of
		// thousands of records and the argument list will not take them.
		list := filepath.Join(tmp, "files.txt")
		var names []byte
		for i := 0; i < n; i++ {
			names = append(names, []byte(filepath.Join(tmp, fmt.Sprintf("%d.json", i))+"\n")...)
		}
		if err := os.WriteFile(list, names, 0o644); err != nil {
			fatal(err)
		}
		cmd := exec.Command("zstd", "--train", "-q", fmt.Sprintf("--maxdict=%d", *size),
			"-o", dst, "--filelist", list)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			os.RemoveAll(tmp)
			fatal(fmt.Errorf("%s: zstd --train: %w", kind, err))
		}
		os.RemoveAll(tmp)
		fi, _ := os.Stat(dst)
		fmt.Fprintf(os.Stderr, "  %-12s %6d records, %7.1f KB source -> %s (%d bytes)\n",
			kind, n, float64(total)/1024, dst, fi.Size())
	}
	fmt.Fprintln(os.Stderr, "\ndictionaries rewritten. The stores built with the previous "+
		"ones can no longer be opened — rebuild them.")
}
