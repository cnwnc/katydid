// Package testaudio creates small tagged flac files for tests.
// Paths are resolved relative to the calling test package's working directory.
package testaudio

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	taglib "go.senan.xyz/taglib"
)

func MakeTracked(t *testing.T, dir, base, title, artist, album string, track, disc, year int) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "tags", "testdata", "short.flac"))
	if err != nil {
		t.Fatalf("read flac fixture: %v", err)
	}
	path := filepath.Join(dir, base)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := taglib.WriteTags(path, map[string][]string{
		"TITLE":       {title},
		"ARTIST":      {artist},
		"ALBUMARTIST": {artist},
		"ALBUM":       {album},
		"TRACKNUMBER": {fmt.Sprint(track)},
		"DISCNUMBER":  {fmt.Sprint(disc)},
		"DATE":        {fmt.Sprint(year)},
	}, 0); err != nil {
		t.Fatalf("write tags %s: %v", path, err)
	}
}
