package discord

import (
	"strings"
	"testing"
)

func TestMenuOptionsTruncatesLabel(t *testing.T) {
	long := strings.Repeat("t", 120)
	options := menuOptions([]Candidate{{ReleaseID: "grp-1", Title: long, Artist: "A", Date: "1969-09-26", TrackCount: 17, PrimaryType: "Album"}}, 8)
	if len(options) != 1 {
		t.Fatalf("options = %+v", options)
	}
	label := []rune(options[0].Label)
	if len(label) != labelLimit {
		t.Fatalf("label length = %d, want %d", len(label), labelLimit)
	}
	if !strings.HasSuffix(options[0].Label, "…") {
		t.Fatalf("label = %q, want ellipsis suffix", options[0].Label)
	}
}

func TestMenuOptionsDescription(t *testing.T) {
	options := menuOptions([]Candidate{{ReleaseID: "grp-1", Title: "Abbey Road", Artist: "The Beatles", Date: "1969-09-26", TrackCount: 17, PrimaryType: "Album"}}, 8)
	if options[0].Description != "1969-09-26, Album, 17 tracks" {
		t.Fatalf("description = %q", options[0].Description)
	}
	if options[0].Value != "grp-1" {
		t.Fatalf("value = %q", options[0].Value)
	}
}

func TestMenuOptionsLimitAndEmptyIDs(t *testing.T) {
	candidates := []Candidate{
		{ReleaseID: "", Title: "skipped"},
		{ReleaseID: "grp-1"},
		{ReleaseID: "grp-2"},
	}
	options := menuOptions(candidates, 1)
	if len(options) != 1 || options[0].Value != "grp-1" {
		t.Fatalf("options = %+v", options)
	}
}

func TestMenuOptionsDescriptionTruncates(t *testing.T) {
	options := menuOptions([]Candidate{{
		ReleaseID:   "grp-1",
		Date:        strings.Repeat("d", 60),
		PrimaryType: strings.Repeat("p", 60),
		TrackCount:  1,
	}}, 8)
	if len([]rune(options[0].Description)) != labelLimit {
		t.Fatalf("description length = %d", len([]rune(options[0].Description)))
	}
}

func TestMatchText(t *testing.T) {
	c := Candidate{ReleaseID: "grp-1", Title: "Abbey Road", Artist: "The Beatles", Date: "1969-09-26", TrackCount: 17, PrimaryType: "Album"}
	text := matchText(c, false)
	want := "Found 1 match: The Beatles - Abbey Road (1969-09-26, Album, 17 tracks)"
	if text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
	if got := matchText(c, true); got != want+"\n"+autoLine {
		t.Fatalf("auto text = %q", got)
	}
}

func TestChoicesText(t *testing.T) {
	spec := addSpec{Artist: "The Beatles", Album: "Abbey Road", Year: 1969}
	if got := choicesText(spec, 3); got != "Found 3 matches for The Beatles - Abbey Road (1969) — pick one:" {
		t.Fatalf("text = %q", got)
	}
	spec.Year = 0
	if got := choicesText(spec, 3); got != "Found 3 matches for The Beatles - Abbey Road — pick one:" {
		t.Fatalf("text = %q", got)
	}
}

func TestParseChoicesSpecRoundTrip(t *testing.T) {
	spec := addSpec{Artist: "The Beatles", Album: "Abbey Road", Year: 1969}
	parsed := parseChoicesSpec(choicesText(spec, 3))
	if parsed.Artist != spec.Artist || parsed.Album != spec.Album || parsed.Year != spec.Year {
		t.Fatalf("parsed = %+v", parsed)
	}
	spec.Year = 0
	parsed = parseChoicesSpec(choicesText(spec, 3))
	if parsed.Year != 0 || parsed.Artist != spec.Artist || parsed.Album != spec.Album {
		t.Fatalf("parsed = %+v", parsed)
	}
}

func TestParseChoicesSpecTitleWithDash(t *testing.T) {
	parsed := parseChoicesSpec("Found 2 matches for MGMT - Oracular Spectacular — pick one:")
	if parsed.Artist != "MGMT" || parsed.Album != "Oracular Spectacular" {
		t.Fatalf("parsed = %+v", parsed)
	}
}

func TestParseChoicesSpecGarbage(t *testing.T) {
	if parsed := parseChoicesSpec("no marker here"); parsed.Artist != "" || parsed.Album != "" {
		t.Fatalf("parsed = %+v", parsed)
	}
}

func TestParseMatchLabel(t *testing.T) {
	c := Candidate{Title: "Abbey Road", Artist: "The Beatles", Date: "1969-09-26", TrackCount: 17, PrimaryType: "Album"}
	if got := parseMatchLabel(matchText(c, false)); got != "The Beatles - Abbey Road" {
		t.Fatalf("label = %q", got)
	}
	if got := parseMatchLabel(matchText(c, true)); got != "The Beatles - Abbey Road" {
		t.Fatalf("label with auto = %q", got)
	}
	if got := parseMatchLabel("unrelated"); got != "unrelated" {
		t.Fatalf("label = %q", got)
	}
}

func TestStripYear(t *testing.T) {
	if s, year := stripYear("Abbey Road (1969)"); s != "Abbey Road" || year != 1969 {
		t.Fatalf("s = %q, year = %d", s, year)
	}
	if s, year := stripYear("Abbey Road"); s != "Abbey Road" || year != 0 {
		t.Fatalf("s = %q, year = %d", s, year)
	}
	if s, year := stripYear("Abbey Road (Deluxe)"); s != "Abbey Road (Deluxe)" || year != 0 {
		t.Fatalf("s = %q, year = %d", s, year)
	}
}

func TestSplitArtistAlbum(t *testing.T) {
	artist, album, ok := splitArtistAlbum("The Beatles - Abbey Road")
	if !ok || artist != "The Beatles" || album != "Abbey Road" {
		t.Fatalf("artist = %q, album = %q, ok = %v", artist, album, ok)
	}
	if _, _, ok := splitArtistAlbum("no separator"); ok {
		t.Fatalf("expected failure without separator")
	}
}
