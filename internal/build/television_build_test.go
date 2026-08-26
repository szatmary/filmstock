package build

import "testing"

// A television film or special has one broadcast rather than a run, and states
// released instead of first_aired. 9,278 pages did that and had no date at all.
func TestReleasedStandsInForFirstAired(t *testing.T) {
	s := buildTelevisionSeries("The Day After", 1, map[string]string{
		"released": "{{Start date|1983|11|20}}",
	})
	if s.FirstAired != "1983-11-20" {
		t.Errorf("first aired %q, want 1983-11-20", s.FirstAired)
	}
}

// first_aired wins where both are stated: it describes the run, released
// usually describes a home-media or festival date.
func TestFirstAiredBeatsReleased(t *testing.T) {
	s := buildTelevisionSeries("X", 1, map[string]string{
		"first_aired": "{{Start date|1994|9|19}}",
		"released":    "{{Start date|2001|1|1}}",
	})
	if s.FirstAired != "1994-09-19" {
		t.Errorf("first aired %q, want 1994-09-19", s.FirstAired)
	}
}
