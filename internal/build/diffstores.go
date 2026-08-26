package build

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/szatmary/filmstock"
)

// Comparing two record trees, record by record.
//
// The immediate use is proving the two-pass split: a record built from the
// intermediate has to be the record extract would have built, and the only way
// to know is to build both and compare. Tests on synthetic pages establish that
// the same bytes reach the same parsers; they cannot establish that 165,740
// films come out the same.
//
// It reports which fields differ, not merely that a record differs. "1,204
// records changed" is unactionable; "1,204 records changed, all of them only in
// seasons[].starring" is a finding.
func CmdDiffStores(args []string) {
	fs := flag.NewFlagSet("diff-stores", flag.ExitOnError)
	kinds := fs.String("kinds", "", "comma-separated kinds to compare (default: all)")
	examples := fs.Int("examples", 3, "example keys to print per differing field")
	full := fs.Bool("full", false, "print the first differing record in full")
	fs.Parse(args)
	if fs.NArg() != 2 {
		fatal(fmt.Errorf("usage: filmstock diff-stores [flags] OLD_DIR NEW_DIR"))
	}
	oldRoot, newRoot := fs.Arg(0), fs.Arg(1)

	want := []string{filmstock.KindMovie, filmstock.KindTelevision,
		filmstock.KindPerson, filmstock.KindEvent}
	if *kinds != "" {
		want = splitComma(*kinds)
	}

	var anyDiff bool
	for _, kind := range want {
		d, err := diffKind(oldRoot, newRoot, kind, *examples, *full)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", kind, err)
			continue
		}
		d.report(kind)
		if d.changed > 0 || d.added > 0 || d.removed > 0 {
			anyDiff = true
		}
	}
	if !anyDiff {
		fmt.Fprintln(os.Stderr, "\nidentical")
	}
}

type storeDiff struct {
	same, changed, added, removed int
	// Which JSON fields differ, and how often. This is the useful output: a
	// difference concentrated in one field is a bug with an address.
	fields   map[string]int
	examples map[string][]string
	firstOld []byte
	firstNew []byte
	firstKey string
}

func diffKind(oldRoot, newRoot, kind string, examples int, full bool) (*storeDiff, error) {
	a, err := filmstock.OpenStore(oldRoot, kind)
	if err != nil {
		return nil, err
	}
	b, err := filmstock.OpenStore(newRoot, kind)
	if err != nil {
		return nil, err
	}

	d := &storeDiff{fields: map[string]int{}, examples: map[string][]string{}}
	seen := map[string]bool{}
	for rec, err := range a.All() {
		if err != nil {
			return nil, err
		}
		seen[rec.Key] = true
		other, err := b.Get(rec.Key)
		if err != nil {
			d.removed++
			continue
		}
		if bytes.Equal(rec.Data, other) {
			d.same++
			continue
		}
		d.changed++
		if d.firstKey == "" {
			d.firstKey, d.firstOld, d.firstNew = rec.Key, rec.Data, other
		}
		for _, f := range differingFields(rec.Data, other) {
			d.fields[f]++
			if len(d.examples[f]) < examples {
				d.examples[f] = append(d.examples[f], rec.Key)
			}
		}
	}
	for _, key := range b.Keys() {
		if !seen[key] {
			d.added++
		}
	}
	if full && d.firstKey != "" {
		fmt.Fprintf(os.Stderr, "\n--- %s %s (old) ---\n%s\n--- (new) ---\n%s\n",
			kind, d.firstKey, indent(d.firstOld), indent(d.firstNew))
	}
	return d, nil
}

// differingFields compares two records as JSON and names the top-level fields
// that differ. Byte comparison alone cannot distinguish a reordered map from a
// changed value, and key order in encoded JSON is not guaranteed to be stable
// across runs — which would report every record as changed.
func differingFields(a, b []byte) []string {
	var ma, mb map[string]json.RawMessage
	if json.Unmarshal(a, &ma) != nil || json.Unmarshal(b, &mb) != nil {
		return []string{"(unparseable)"}
	}
	var out []string
	for k, va := range ma {
		vb, ok := mb[k]
		if !ok {
			out = append(out, k+" (removed)")
			continue
		}
		if !jsonEqual(va, vb) {
			out = append(out, k)
		}
	}
	for k := range mb {
		if _, ok := ma[k]; !ok {
			out = append(out, k+" (added)")
		}
	}
	sort.Strings(out)
	return out
}

// jsonEqual compares two JSON values by structure rather than by bytes, so a
// map written in a different order is not reported as a change.
func jsonEqual(a, b json.RawMessage) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return bytes.Equal(a, b)
	}
	ca, err1 := json.Marshal(canon(va))
	cb, err2 := json.Marshal(canon(vb))
	if err1 != nil || err2 != nil {
		return bytes.Equal(a, b)
	}
	return bytes.Equal(ca, cb)
}

// canon rewrites a decoded JSON value so that encoding it is deterministic.
// Go's json.Marshal already sorts map keys, so this only has to rebuild the
// nested maps it produced from interface values.
func canon(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = canon(vv)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = canon(vv)
		}
		return out
	}
	return v
}

func (d *storeDiff) report(kind string) {
	fmt.Fprintf(os.Stderr, "\n%s: %d identical, %d changed, %d only in new, %d only in old\n",
		kind, d.same, d.changed, d.added, d.removed)
	if len(d.fields) == 0 {
		return
	}
	type fc struct {
		f string
		n int
	}
	var fcs []fc
	for f, n := range d.fields {
		fcs = append(fcs, fc{f, n})
	}
	sort.Slice(fcs, func(i, j int) bool { return fcs[i].n > fcs[j].n })
	for _, f := range fcs {
		fmt.Fprintf(os.Stderr, "  %-28s %7d  e.g. %v\n", f.f, f.n, d.examples[f.f])
	}
}

func indent(b []byte) string {
	var buf bytes.Buffer
	if json.Indent(&buf, b, "", "  ") != nil {
		return string(b)
	}
	return buf.String()
}

func splitComma(s string) []string {
	var out []string
	for _, p := range bytes.Split([]byte(s), []byte(",")) {
		if t := string(bytes.TrimSpace(p)); t != "" {
			out = append(out, t)
		}
	}
	return out
}
