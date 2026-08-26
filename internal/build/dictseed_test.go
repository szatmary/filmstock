package build

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Emits parsed schedules as JSON so a compression dictionary can be trained on
// real records rather than invented content. Run explicitly:
//
//	go test ./internal/build -run DumpScheduleSamples -args -dump=/tmp/sched-samples
func TestDumpScheduleSamples(t *testing.T) {
	out := os.Getenv("SCHEDULE_SAMPLE_DIR")
	if out == "" {
		t.Skip("set SCHEDULE_SAMPLE_DIR to emit samples")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, y := range []string{"1955-56", "1974-75", "1996-97", "2015-16", "2023-24"} {
		s := buildSchedule(*loadSchedule(t, y))
		if s == nil {
			continue
		}
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, y+".json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("  %s: %d entries, %d bytes of JSON", y, len(s.Entries), len(b))
	}
}
