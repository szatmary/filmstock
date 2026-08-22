package main

import "testing"

// The collision that put 4,065 non-films into the film index. Both of these
// begin with "Infobox film", so any prefix test classifies them as films.
func TestFilmAwardsAndFestivalsAreNotFilms(t *testing.T) {
	cases := []struct {
		name string
		text string
		film bool
		kind string
	}{
		{"film", "{{Infobox film\n| name = The Matrix\n| director = [[The Wachowskis]]\n}}", true, ""},
		{"film no space", "{{Infobox film|name=Jaws|director=[[Steven Spielberg]]}}", true, ""},
		{"awards", "{{Infobox film awards\n| award = Golden Raspberry Awards\n| number = 1\n| host = [[Bob Guy]]\n| network = [[NBC]]\n}}", false, eventAwardCeremony},
		{"festival", "{{Infobox Film festival\n| name = Cannes\n| number = 70\n| location = Cannes\n| opening = [[Ismael's Ghosts]]\n}}", false, eventFilmFestival},
	}
	for _, c := range cases {
		p := Page{Title: c.name, ID: 1, NS: 0, Text: c.text}
		if got := buildFilm(p) != nil; got != c.film {
			t.Errorf("%s: buildFilm non-nil = %v, want %v", c.name, got, c.film)
		}
		ev := buildEvent(p)
		if c.kind == "" {
			if ev != nil {
				t.Errorf("%s: buildEvent returned %+v, want nil", c.name, ev)
			}
			continue
		}
		if ev == nil {
			t.Fatalf("%s: buildEvent returned nil, want %s", c.name, c.kind)
		}
		if ev.Kind != c.kind {
			t.Errorf("%s: kind = %q, want %q", c.name, ev.Kind, c.kind)
		}
	}
}

func TestEventFieldsParsed(t *testing.T) {
	p := Page{Title: "53rd NAACP Image Awards", ID: 69815195, NS: 0, Text: `{{Infobox film awards
| award = NAACP Image Award
| date = February 26, 2022
| site = [[Pasadena Civic Auditorium]]
| host = [[Anthony Anderson]]
| network = [[BET]]
| most_wins = ''King Richard'' (3)
}}
The '''53rd NAACP Image Awards''' honored the best in film.`}
	e := buildEvent(p)
	if e == nil {
		t.Fatal("buildEvent returned nil")
	}
	if e.Edition != 53 {
		t.Errorf("edition = %d, want 53 (from the title ordinal)", e.Edition)
	}
	if e.Year != 2022 {
		t.Errorf("year = %d, want 2022 (from the date)", e.Year)
	}
	if len(e.Hosts) != 1 || e.Hosts[0].Name != "Anthony Anderson" {
		t.Errorf("hosts = %+v, want [Anthony Anderson]", e.Hosts)
	}
	if len(e.Network) != 1 || e.Network[0] != "BET" {
		t.Errorf("network = %v, want [BET]", e.Network)
	}
	if e.Venue == "" {
		t.Error("venue is empty")
	}
}

func TestNetworkLabelsStripped(t *testing.T) {
	got := networkList("Broadcast: [[American Broadcasting Company|ABC]]<br>Streaming: [[Hulu]]")
	if len(got) != 2 || got[0] != "ABC" || got[1] != "Hulu" {
		t.Errorf("networkList = %v, want [ABC Hulu]", got)
	}
}

// A festival's "host" is the organising body, not a presenter.
func TestFestivalHostIsNotAPerson(t *testing.T) {
	fest := buildEvent(Page{Title: "Toronto International Film Festival", ID: 2, NS: 0,
		Text: "{{Infobox Film festival\n| name = TIFF\n| host = Toronto International Film Festival Group\n}}"})
	if fest == nil {
		t.Fatal("nil event")
	}
	if len(fest.Hosts) != 0 {
		t.Errorf("festival produced %d Person hosts, want 0: %+v", len(fest.Hosts), fest.Hosts)
	}
	if fest.Organizer != "Toronto International Film Festival Group" {
		t.Errorf("organizer = %q", fest.Organizer)
	}
	cer := buildEvent(Page{Title: "76th Academy Awards", ID: 3, NS: 0,
		Text: "{{Infobox film awards\n| award = Academy Award\n| host = [[Billy Crystal]]\n}}"})
	if cer == nil || len(cer.Hosts) != 1 || cer.Hosts[0].Name != "Billy Crystal" {
		t.Errorf("ceremony hosts = %+v, want [Billy Crystal]", cer)
	}
	if cer.Organizer != "" {
		t.Errorf("ceremony organizer = %q, want empty", cer.Organizer)
	}
}
