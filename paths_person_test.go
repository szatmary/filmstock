package filmstock

import "testing"

// The pair that actually collided. Two exports of identical input put both of
// these under key -2070761073, and gitdb keys are unique, so one of them was
// not in the database at all.
func TestTheObservedCollisionIsGone(t *testing.T) {
	a := PersonRecordPathID("Issa Abdessamie")
	b := PersonRecordPathID("Costache Ciubotaru")
	if a == b {
		t.Fatalf("still colliding at %d", a)
	}
}

// The key is negated to keep it away from page_ids, so it must fit in an int64
// with room for the sign.
func TestKeyFitsInASignedInt64(t *testing.T) {
	for _, s := range []string{"", "a", "Issa Abdessamie",
		"A rather long link target with punctuation, ampersands & such (1999)"} {
		h := PersonRecordPathID(s)
		if h > 1<<63-1 {
			t.Errorf("%q hashes to %d, which does not fit signed", s, h)
		}
		if got := -int64(h); got > 0 {
			t.Errorf("%q negates to %d, which collides with the page_id space", s, got)
		}
	}
}

// Stable across runs and processes: the key is a record's path on disk.
func TestKeyIsStable(t *testing.T) {
	const want = "Some Uncredited Extra"
	first := PersonRecordPathID(want)
	for range 100 {
		if PersonRecordPathID(want) != first {
			t.Fatal("hash is not stable")
		}
	}
}

// A sanity check on the spread, at the scale that broke the 31-bit version.
func TestNoCollisionsAcrossManyNames(t *testing.T) {
	seen := make(map[uint64]string, 200000)
	for i := range 200000 {
		name := "Person " + string(rune('A'+i%26)) + itoa(i)
		h := PersonRecordPathID(name)
		if prev, dup := seen[h]; dup {
			t.Fatalf("collision at %d: %q and %q", h, prev, name)
		}
		seen[h] = name
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
