package main

import (
	"strings"
	"testing"

	"doppel.moe/katydid/internal/ansi"
	"doppel.moe/katydid/internal/importer"
	"doppel.moe/katydid/internal/library"
)

func testAlbum() library.Album {
	return library.Album{
		ID:      "Gaza/2012 - No Absolutes in Human Suffering",
		Pending: false,
		Meta: library.Meta{
			AlbumArtist: "Gaza",
			Album:       "No Absolutes in Human Suffering",
			Year:        2012,
			Tracks: []library.TrackMeta{
				{File: "a.flac", Title: "a"},
				{File: "b.flac", Title: "b"},
			},
		},
	}
}

func TestRenderAlbumFormat(t *testing.T) {
	album := testAlbum()

	cases := []struct {
		template string
		want     string
	}{
		{defaultListFormat, "2012 Gaza - No Absolutes in Human Suffering"},
		{"{path}: {album} by {artist}", "Gaza/2012 - No Absolutes in Human Suffering: No Absolutes in Human Suffering by Gaza"},
		{"{album} by {artist} ({year})", "No Absolutes in Human Suffering by Gaza (2012)"},
		{"{artist} — {album} [{tracks}]", "Gaza — No Absolutes in Human Suffering [2]"},
		{"{year} {artist}", "2012 Gaza"},
	}
	for _, tc := range cases[:5] {
		got, err := renderAlbumFormat(tc.template, album)
		if err != nil {
			t.Errorf("%q: %v", tc.template, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.template, got, tc.want)
		}
	}
}

func TestRenderAlbumFormatUnknownField(t *testing.T) {
	_, err := renderAlbumFormat("{album} by {bogus}", testAlbum())
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Errorf("unknown field: got %v, want error naming bogus", err)
	}
}

func TestRenderAlbumFormatZeroYearOmits(t *testing.T) {
	album := testAlbum()
	album.Meta.Year = 0
	got, err := renderAlbumFormat(defaultListFormat, album)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got != "Gaza - No Absolutes in Human Suffering" {
		t.Errorf("zero year: got %q, want no leading space or 0", got)
	}
}

func TestStyleNoteBody(t *testing.T) {
	prev := ansi.Enabled()
	ansi.SetEnabled(true)
	defer ansi.SetEnabled(prev)

	cases := []struct{ note, want string }{
		{"title: First Song -> 最初の歌", "\x1b[2mtitle: \x1b[0mFirst Song\x1b[2m -> \x1b[0m\x1b[32m最初の歌\x1b[0m"},
		{"left out 1 file(s) not part of the release: 03 - Outtake.flac", "\x1b[33mleft out 1 file(s) not part of the release: 03 - Outtake.flac\x1b[0m"},
		{"top candidate Test Artist - Test Album rejected: boom", "\x1b[31mtop candidate Test Artist - Test Album rejected: boom\x1b[0m"},
		{"previous album moved to .trash", "previous album moved to .trash"},
	}
	for _, tc := range cases {
		if got := styleNoteBody(tc.note); got != tc.want {
			t.Errorf("styleNoteBody(%q): got %q, want %q", tc.note, got, tc.want)
		}
	}
}

func TestStyleNoteBodyPlainWhenDisabled(t *testing.T) {
	prev := ansi.Enabled()
	ansi.SetEnabled(false)
	defer ansi.SetEnabled(prev)

	for _, note := range []string{"title: A -> B", "left out 1 file(s)", "rejected: x", "plain"} {
		if got := styleNoteBody(note); got != note {
			t.Errorf("styleNoteBody(%q): got %q, want unchanged", note, got)
		}
	}
}

func TestRemapLine(t *testing.T) {
	prev := ansi.Enabled()
	defer ansi.SetEnabled(prev)

	table := &importer.Remap{
		Files:  []importer.RemapFile{{Index: 1, File: "01 Intro.flac"}, {Index: 2, File: "02 Extra.flac"}},
		Tracks: []importer.RemapTrack{{Index: 1, Number: "1", Title: "Intro"}},
		Map:    map[int]int{1: 1},
	}
	ansi.SetEnabled(true)
	paired := "  \x1b[36m01: \"01 Intro.flac\"\x1b[0m\x1b[2m -> \x1b[0m\x1b[32m1 Intro\x1b[0m"
	unpaired := "  \x1b[36m02: \"02 Extra.flac\"\x1b[0m\x1b[2m (unpaired)\x1b[0m"
	if got := remapLine(table, table.Files[0]); got != paired {
		t.Errorf("paired: got %q, want %q", got, paired)
	}
	if got := remapLine(table, table.Files[1]); got != unpaired {
		t.Errorf("unpaired: got %q, want %q", got, unpaired)
	}

	ansi.SetEnabled(false)
	plain := "  01: \"01 Intro.flac\" -> 1 Intro"
	if got := remapLine(table, table.Files[0]); got != plain {
		t.Errorf("disabled: got %q, want %q", got, plain)
	}
}
