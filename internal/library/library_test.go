package library

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"doppel.moe/katydid/internal/sidecar"
)

const scratchRoot = "/srv/git/katydid/scratch/original"

func fixtureFlac(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "tags", "testdata", "short.flac"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

func writeLibrary(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	gaza := filepath.Join(root, "Gaza", "2012 - No Absolutes in Human Suffering")
	if err := os.MkdirAll(gaza, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, file := range []string{"01 - Mostly Hair and Bones Now.flac", "02 - This We Celebrate.flac"} {
		if err := os.WriteFile(filepath.Join(gaza, file), []byte("not audio"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	drukqs := filepath.Join(root, "Aphex Twin", "2001 - Drukqs")
	if err := os.MkdirAll(drukqs, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, file := range []string{"01 - Jynweythek.flac", "02 - Vordhosbn.flac"} {
		if err := os.WriteFile(filepath.Join(drukqs, file), fixtureFlac(t), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	if err := sidecar.Save(gaza, &sidecar.Album{
		Album:       "No Absolutes in Human Suffering",
		AlbumArtist: "Gaza",
		Year:        2012,
		Provenance:  sidecar.Provenance{Imported: time.Now(), By: "test"},
		Tracks: []sidecar.Track{
			{File: "01 - Mostly Hair and Bones Now.flac", Title: "Mostly Hair and Bones Now", Track: 1, LengthSeconds: 155},
			{File: "02 - This We Celebrate.flac", Title: "This We Celebrate", Track: 2, LengthSeconds: 201},
		},
	}); err != nil {
		t.Fatalf("save sidecar: %v", err)
	}
	return root
}

func TestScanGroupsAlbums(t *testing.T) {
	root := writeLibrary(t)
	ix := NewIndex(root)
	if err := ix.Scan(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	albums := ix.Albums(Query{})
	if len(albums) != 2 {
		t.Fatalf("albums: got %d, want 2", len(albums))
	}
	if albums[0].Meta.AlbumArtist != "Aphex Twin" || albums[1].Meta.AlbumArtist != "Gaza" {
		t.Errorf("sort order: got %s then %s", albums[0].Meta.AlbumArtist, albums[1].Meta.AlbumArtist)
	}
	if albums[1].ID != filepath.Join("Gaza", "2012 - No Absolutes in Human Suffering") {
		t.Errorf("id: got %q", albums[1].ID)
	}
}

func TestScanSidecarAndPending(t *testing.T) {
	root := writeLibrary(t)
	ix := NewIndex(root)
	if err := ix.Scan(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	gaza, err := ix.Album(filepath.Join("Gaza", "2012 - No Absolutes in Human Suffering"))
	if err != nil {
		t.Fatalf("album: %v", err)
	}
	if gaza.Pending {
		t.Errorf("gaza: got pending, want sidecar-backed")
	}
	if gaza.Meta.Year != 2012 || len(gaza.Meta.Tracks) != 2 || gaza.Meta.Tracks[0].Title != "Mostly Hair and Bones Now" {
		t.Errorf("gaza meta: %+v", gaza.Meta)
	}

	drukqs, err := ix.Album(filepath.Join("Aphex Twin", "2001 - Drukqs"))
	if err != nil {
		t.Fatalf("album: %v", err)
	}
	if !drukqs.Pending {
		t.Errorf("drukqs: got sidecar-backed, want pending")
	}
	if drukqs.Meta.Album != "Drukqs" || drukqs.Meta.AlbumArtist != "Aphex Twin" || drukqs.Meta.Year != 2001 {
		t.Errorf("drukqs meta from tags: %+v", drukqs.Meta)
	}
	if len(drukqs.Meta.Tracks) != 2 || drukqs.Meta.Tracks[0].Title != "Lornaderek" {
		t.Errorf("drukqs tracks (fixture tags): %+v", drukqs.Meta.Tracks)
	}
}

func TestQuery(t *testing.T) {
	root := writeLibrary(t)
	ix := NewIndex(root)
	if err := ix.Scan(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if got := ix.Albums(Query{Q: "lornaderek"}); len(got) != 1 || got[0].Meta.Album != "Drukqs" {
		t.Errorf("query by track title: got %+v", got)
	}
	if got := ix.Albums(Query{Q: "this we celebrate"}); len(got) != 1 || got[0].Meta.Album != "No Absolutes in Human Suffering" {
		t.Errorf("query by sidecar track title: got %+v", got)
	}
	if got := ix.Albums(Query{Artist: "gaza"}); len(got) != 1 || got[0].Meta.AlbumArtist != "Gaza" {
		t.Errorf("query by artist: got %+v", got)
	}
	if got := ix.Albums(Query{Year: 2012}); len(got) != 1 || got[0].Meta.Album != "No Absolutes in Human Suffering" {
		t.Errorf("query by year: got %+v", got)
	}
	if got := ix.Albums(Query{Year: 1999}); len(got) != 0 {
		t.Errorf("query by absent year: got %+v, want none", got)
	}
}

func TestCheck(t *testing.T) {
	root := writeLibrary(t)
	ix := NewIndex(root)
	if err := ix.Scan(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	findings := ix.Check()
	if len(findings) != 1 || findings[0].Kind != KindPendingImport || findings[0].Album != filepath.Join("Aphex Twin", "2001 - Drukqs") {
		t.Fatalf("check before mutation: got %+v", findings)
	}

	gazaDir := filepath.Join(root, "Gaza", "2012 - No Absolutes in Human Suffering")
	if err := os.Remove(filepath.Join(gazaDir, "01 - Mostly Hair and Bones Now.flac")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gazaDir, "bonus.flac"), []byte("not audio"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := ix.Scan(); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	findings = ix.Check()
	byKind := map[string]string{}
	for _, f := range findings {
		if f.Album == filepath.Join("Gaza", "2012 - No Absolutes in Human Suffering") {
			byKind[f.Kind] = f.File
		}
	}
	if byKind[KindMissingFile] != "01 - Mostly Hair and Bones Now.flac" {
		t.Errorf("missing file finding: got %+v", findings)
	}
	if byKind[KindUnlistedFile] != "bonus.flac" {
		t.Errorf("unlisted file finding: got %+v", findings)
	}
}

func TestScanCorruptSidecarFailsLoud(t *testing.T) {
	root := writeLibrary(t)
	bad := filepath.Join(root, "Gaza", "2012 - No Absolutes in Human Suffering", "album.yaml")
	if err := os.WriteFile(bad, []byte("schema: 1\nalbum: x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := NewIndex(root).Scan(); err == nil {
		t.Errorf("scan with corrupt sidecar: got nil error, want failure")
	}
}

func TestScanScratchIntegration(t *testing.T) {
	if _, err := os.Stat(scratchRoot); err != nil {
		t.Skip("scratch fixture not present")
	}
	ix := NewIndex(scratchRoot)
	if err := ix.Scan(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if got := len(ix.Albums(Query{})); got != 3 {
		t.Errorf("albums: got %d, want 3", got)
	}
	status := ix.Status()
	if status.Files != 50 {
		t.Errorf("files: got %d, want 50", status.Files)
	}

	drukqs, err := ix.Album("Drukqs")
	if err != nil {
		t.Fatalf("album: %v", err)
	}
	if !drukqs.Pending || drukqs.Meta.Year != 2001 || len(drukqs.Meta.Tracks) != 30 {
		t.Errorf("drukqs: pending=%v year=%d tracks=%d", drukqs.Pending, drukqs.Meta.Year, len(drukqs.Meta.Tracks))
	}
	if drukqs.Meta.Tracks[0].Title != "Jynweythek" || drukqs.Meta.Tracks[0].Track != 1 {
		t.Errorf("drukqs first track: %+v", drukqs.Meta.Tracks[0])
	}

	saetia, err := ix.Album("Saetia")
	if err != nil {
		t.Fatalf("album: %v", err)
	}
	if saetia.Meta.Year != 1998 {
		t.Errorf("saetia year from tags: got %d, want 1998", saetia.Meta.Year)
	}
}
