package sidecar

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := Album{
		Album:       "No Absolutes in Human Suffering",
		AlbumArtist: "Gaza",
		Year:        2012,
		MusicBrainz: MusicBrainz{ReleaseGroupID: "6013cf3f", ReleaseID: "d021d222"},
		Provenance: Provenance{
			Imported: time.Date(2026, 9, 24, 7, 0, 0, 0, time.UTC),
			By:       "sana",
			Request:  "add No Absolutes in Human Suffering by Gaza",
			Origin:   Origin{Path: "/srv/data/music-archive"},
		},
		TagState: &TagState{Policy: "default", Applied: time.Date(2026, 9, 24, 7, 0, 5, 0, time.UTC), StateHash: "sha256:abc"},
		Tracks: []Track{
			{File: "01 - Mostly Hair and Bones Now.flac", Title: "Mostly Hair and Bones Now", Track: 1, LengthSeconds: 155},
			{File: "02 - This We Celebrate.flac", Title: "This We Celebrate", Track: 2, Disc: 2, LengthSeconds: 201},
		},
	}

	if err := Save(dir, &in); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if out.Album != in.Album || out.AlbumArtist != in.AlbumArtist || out.Year != in.Year {
		t.Errorf("round trip mismatch: %+v", out)
	}
	if out.MusicBrainz != in.MusicBrainz {
		t.Errorf("musicbrainz: got %+v, want %+v", out.MusicBrainz, in.MusicBrainz)
	}
	if len(out.Tracks) != 2 || out.Tracks[1].Disc != 2 || out.Tracks[0].LengthSeconds != 155 {
		t.Errorf("tracks: got %+v", out.Tracks)
	}
	if out.TagState == nil || out.TagState.StateHash != "sha256:abc" {
		t.Errorf("tag state: got %+v", out.TagState)
	}
	if !out.Provenance.Imported.Equal(in.Provenance.Imported) {
		t.Errorf("imported: got %v, want %v", out.Provenance.Imported, in.Provenance.Imported)
	}
}

func TestLoadMissing(t *testing.T) {
	if _, err := Load(t.TempDir()); !errors.Is(err, ErrNoSidecar) {
		t.Errorf("load missing: got %v, want ErrNoSidecar", err)
	}
}

func TestLoadRejectsNewerSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte("schema: 99\nalbum: x\nalbumartist: y\ntracks: []\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(dir); err == nil {
		t.Errorf("load newer schema: got nil error, want failure")
	}
}

func TestLoadToleratesUnknownFields(t *testing.T) {
	dir := t.TempDir()
	body := "schema: 1\nalbum: x\nalbumartist: y\ntracks:\n  - {file: a.flac, title: a, track: 1}\nsomethingnew: 1\n"
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(dir); err != nil {
		t.Errorf("load unknown field: got %v, want success", err)
	}
}

func TestSaveRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]Album{
		"empty album":        {AlbumArtist: "x", Tracks: []Track{{File: "a.flac", Title: "a"}}},
		"empty albumartist":  {Album: "x", Tracks: []Track{{File: "a.flac", Title: "a"}}},
		"no tracks":          {Album: "x", AlbumArtist: "y"},
		"track missing file": {Album: "x", AlbumArtist: "y", Tracks: []Track{{Title: "a"}}},
		"track missing title": {Album: "x", AlbumArtist: "y", Tracks: []Track{{File: "a.flac"}}},
		"duplicate files":    {Album: "x", AlbumArtist: "y", Tracks: []Track{{File: "a.flac", Title: "a"}, {File: "a.flac", Title: "b"}}},
	}
	for name, album := range cases {
		if err := Save(dir, &album); err == nil {
			t.Errorf("%s: got nil error, want failure", name)
		}
	}
}
