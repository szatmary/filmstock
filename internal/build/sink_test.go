package build

import "sync"

// memSink is the test recordSink: it just remembers what was put.
type memSink struct {
	mu   sync.Mutex
	recs map[string]map[int64]any
}

func newMemSink() *memSink { return &memSink{recs: map[string]map[int64]any{}} }

func (m *memSink) put(kind string, identity int64, v any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.recs[kind] == nil {
		m.recs[kind] = map[int64]any{}
	}
	m.recs[kind][identity] = v
}
func (m *memSink) sweep() int              { return 0 }
func (m *memSink) Counts() (int, int, int) { return 0, 0, 0 }
func (m *memSink) Err() error              { return nil }
