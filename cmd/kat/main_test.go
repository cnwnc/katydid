package main

import (
	"strings"
	"testing"

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
