package tags

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

const scratchRoot = "/srv/git/katydid/scratch/original"

func TestReadFixture(t *testing.T) {
	tags, err := Read(filepath.Join("testdata", "short.flac"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if tags.Title != "Lornaderek" {
		t.Errorf("title: got %q, want %q", tags.Title, "Lornaderek")
	}
	if tags.Album != "Drukqs" {
		t.Errorf("album: got %q, want %q", tags.Album, "Drukqs")
	}
	if len(tags.Artists) != 1 || tags.Artists[0] != "Aphex Twin" {
		t.Errorf("artists: got %v, want [Aphex Twin]", tags.Artists)
	}
	if tags.TrackNumber != 18 {
		t.Errorf("track number: got %d, want 18", tags.TrackNumber)
	}
	if tags.ReleaseID == "" {
		t.Errorf("release id: got empty, want a musicbrainz id")
	}
	if tags.SampleRate != 44100 {
		t.Errorf("sample rate: got %d, want 44100", tags.SampleRate)
	}
	if tags.LengthSeconds < 30 || tags.LengthSeconds > 33 {
		t.Errorf("length seconds: got %f, want ~31", tags.LengthSeconds)
	}
}

func TestProbeScratch(t *testing.T) {
	if _, err := os.Stat(scratchRoot); err != nil {
		t.Skip("scratch fixture not present")
	}
	probe := []string{
		"Drukqs/18 Lornaderek.flac",
		"Drukqs/10 Mt Saint Michel + Saint Michaels Mount.flac",
		"No Absolutes in Human Suffering/01 Mostly Hair and Bones Now.flac",
		"Saetia/01 Notres Langues Nous Trompes.flac",
	}
	for _, rel := range probe {
		tags, err := Read(filepath.Join(scratchRoot, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		fmt.Printf("PROBE %s\n  title=%q album=%q artists=%v album_artists=%v\n  track=%d/%d disc=%d/%d year=%d orig_year=%d\n  genres=%v label=%q catnum=%q rgid=%q relid=%q\n  len=%.1fs rate=%d depth=%d ch=%d\n",
			rel, tags.Title, tags.Album, tags.Artists, tags.AlbumArtists,
			tags.TrackNumber, tags.TrackTotal, tags.DiscNumber, tags.DiscTotal,
			tags.Year, tags.OriginalYear, tags.Genres, tags.Label, tags.CatalogNumber,
			tags.ReleaseGroupID, tags.ReleaseID,
			tags.LengthSeconds, tags.SampleRate, tags.BitDepth, tags.Channels)
	}
}
