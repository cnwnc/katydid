package cli_test

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"doppel.moe/katydid/internal/api"
	"doppel.moe/katydid/internal/cli"
	"doppel.moe/katydid/internal/library"
	"doppel.moe/katydid/internal/sidecar"
)

func startDaemon(t *testing.T) (*cli.Client, library.Status) {
	t.Helper()
	root := t.TempDir()
	albumDir := filepath.Join(root, "Gaza", "2012 - No Absolutes in Human Suffering")
	if err := os.MkdirAll(albumDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, file := range []string{"01 - Mostly Hair and Bones Now.flac", "02 - This We Celebrate.flac"} {
		if err := os.WriteFile(filepath.Join(albumDir, file), []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := sidecar.Save(albumDir, &sidecar.Album{
		Album:       "No Absolutes in Human Suffering",
		AlbumArtist: "Gaza",
		Year:        2012,
		Provenance:  sidecar.Provenance{Imported: time.Now(), By: "test"},
		Tracks: []sidecar.Track{
			{File: "01 - Mostly Hair and Bones Now.flac", Title: "Mostly Hair and Bones Now", Track: 1, LengthSeconds: 155},
			{File: "02 - This We Celebrate.flac", Title: "This We Celebrate", Track: 2, LengthSeconds: 201},
		},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	index := library.NewIndex(root)
	if err := index.Scan(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	socket := filepath.Join(t.TempDir(), "test.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: (&api.Server{Index: index}).Handler()}
	go server.Serve(listener)
	t.Cleanup(func() {
		server.Close()
		os.Remove(socket)
	})
	return cli.Dial(socket), index.Status()
}

func TestEndToEnd(t *testing.T) {
	client, want := startDaemon(t)

	status, err := client.Status()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Status.Albums != want.Albums || status.Status.Files != want.Files {
		t.Errorf("status: got %+v, want albums=%d files=%d", status.Status, want.Albums, want.Files)
	}

	albums, err := client.Albums(library.Query{Artist: "gaza"})
	if err != nil {
		t.Fatalf("albums: %v", err)
	}
	if len(albums.Albums) != 1 || albums.Albums[0].Meta.Album != "No Absolutes in Human Suffering" {
		t.Errorf("albums: got %+v", albums.Albums)
	}

	album, err := client.Album("Gaza/2012 - No Absolutes in Human Suffering")
	if err != nil {
		t.Fatalf("album: %v", err)
	}
	if album.Pending || len(album.Meta.Tracks) != 2 {
		t.Errorf("album: got %+v", album)
	}

	if _, err := client.Album("missing"); err == nil {
		t.Errorf("album missing: got nil error, want failure")
	}

	if _, err := client.Albums(library.Query{Year: -5}); err == nil {
		t.Errorf("negative year: got nil error, want failure")
	}
}

func TestScanEndpoint(t *testing.T) {
	client, _ := startDaemon(t)
	scan, err := client.Scan()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scan.Status.Albums != 1 {
		t.Errorf("scan status: got %+v", scan.Status)
	}
}

func TestCheckEndpoint(t *testing.T) {
	client, _ := startDaemon(t)
	check, err := client.Check()
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(check.Findings) != 0 {
		t.Errorf("check: got findings %+v, want none", check.Findings)
	}
}

func TestErrorShape(t *testing.T) {
	client, _ := startDaemon(t)
	_, err := client.Album("nope")
	if err == nil {
		t.Fatalf("album nope: got nil error, want failure")
	}
	var errBodyWant = "GET /album?id=nope: no album with id nope"
	if err.Error() != errBodyWant {
		t.Errorf("error shape: got %q, want %q", err.Error(), errBodyWant)
	}
}
