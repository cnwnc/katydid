package policy

import (
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func defaultPolicy(t *testing.T) Policy {
	t.Helper()
	p, err := Default().Policy("default")
	if err != nil {
		t.Fatalf("default policy: %v", err)
	}
	return p
}

func TestApplyWhitelistScrubs(t *testing.T) {
	p := defaultPolicy(t)
	raw := map[string][]string{
		"TITLE":                 {"Jynweythek"},
		"ARTIST":                {"Aphex Twin"},
		"COMMENT":               {"ripped by x"},
		"REPLAYGAIN_TRACK_GAIN": {"-7.00 dB"},
		"MUSICBRAINZ_ALBUMID":   {"rel-1"},
	}
	out := Apply(p, raw)
	if _, ok := out["COMMENT"]; ok {
		t.Errorf("comment survived the scrub")
	}
	if _, ok := out["REPLAYGAIN_TRACK_GAIN"]; ok {
		t.Errorf("replaygain survived the scrub")
	}
	if len(out["TITLE"]) != 1 || out["TITLE"][0] != "Jynweythek" {
		t.Errorf("title: %v", out["TITLE"])
	}
	if len(out["MUSICBRAINZ_ALBUMID"]) != 1 || out["MUSICBRAINZ_ALBUMID"][0] != "rel-1" {
		t.Errorf("mb id: %v", out["MUSICBRAINZ_ALBUMID"])
	}
}

func TestApplyFeatSplit(t *testing.T) {
	p := defaultPolicy(t)
	out := Apply(p, map[string][]string{
		"ARTIST":      {"Charmer feat. See You At Six"},
		"ALBUMARTIST": {"Self Esteem ft. someone, other & third"},
	})
	wantArtist := []string{"Charmer", "See You At Six"}
	if len(out["ARTIST"]) != 2 || out["ARTIST"][0] != wantArtist[0] || out["ARTIST"][1] != wantArtist[1] {
		t.Errorf("artist split: %v", out["ARTIST"])
	}
	wantAlbum := []string{"Self Esteem", "someone", "other", "third"}
	if len(out["ALBUMARTIST"]) != 4 {
		t.Errorf("albumartist split: %v", out["ALBUMARTIST"])
	}
	for i, want := range wantAlbum {
		if out["ALBUMARTIST"][i] != want {
			t.Errorf("albumartist[%d]: got %q, want %q", i, out["ALBUMARTIST"][i], want)
		}
	}

	plain := Apply(p, map[string][]string{"ARTIST": {"Simon & Garfunkel"}})
	if len(plain["ARTIST"]) != 1 || plain["ARTIST"][0] != "Simon & Garfunkel" {
		t.Errorf("plain ampersand must not split: %v", plain["ARTIST"])
	}

	noSplit := defaultPolicy(t)
	noSplit.FeatSplit = false
	kept := Apply(noSplit, map[string][]string{"ARTIST": {"A feat. B"}})
	if len(kept["ARTIST"]) != 1 || kept["ARTIST"][0] != "A feat. B" {
		t.Errorf("feat_split=false must keep the raw value: %v", kept["ARTIST"])
	}
}

func TestPlanFilename(t *testing.T) {
	p := defaultPolicy(t)

	name, err := PlanFilename(p, 1, 3, "The Truth Weighs Nothing", ".flac", 1)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if name != "03 - The Truth Weighs Nothing.flac" {
		t.Errorf("single disc name: got %q", name)
	}

	name, err = PlanFilename(p, 2, 3, "Lornaderek", ".flac", 2)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if name != "2-03 - Lornaderek.flac" {
		t.Errorf("multi disc name: got %q", name)
	}

	name, err = PlanFilename(p, 1, 7, "Band / Band / Band", ".FLAC", 1)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if name != "07 - Band - Band - Band.flac" {
		t.Errorf("unsafe title: got %q", name)
	}

	if _, err := PlanFilename(p, 1, 1, "x", ".flac", 1); err != nil {
		t.Errorf("plan with defaults: %v", err)
	}
}

func TestPlanDir(t *testing.T) {
	p := defaultPolicy(t)
	dir, err := PlanDir(p, "Aphex Twin", "Drukqs", 2001)
	if err != nil {
		t.Fatalf("plan dir: %v", err)
	}
	if dir != filepath.Join("Aphex Twin", "2001 - Drukqs") {
		t.Errorf("dir: got %q", dir)
	}
	dir, err = PlanDir(p, "AC/DC", "...", 0)
	if err != nil {
		t.Fatalf("plan dir: %v", err)
	}
	if dir != filepath.Join("AC-DC", "0 - Unknown Album") {
		t.Errorf("dir fallbacks: got %q", dir)
	}
}

func TestLoadDefaultsWhenMissing(t *testing.T) {
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("load missing: %v", err)
	}
	if cfg.Default != "default" || len(cfg.Policies) != 1 {
		t.Errorf("defaults: %+v", cfg)
	}
}

func TestEnsureWritesFile(t *testing.T) {
	root := t.TempDir()
	cfg, err := Ensure(root)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, FileName)); err != nil {
		t.Fatalf("musiclib.yaml not written: %v", err)
	}

	edited := cfg
	defaultPolicy := edited.Policies["default"]
	defaultPolicy.File = "{track} - {title}{ext}"
	edited.Policies["default"] = defaultPolicy
	data, err := yaml.Marshal(edited)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, FileName), data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	loaded, err := Load(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Policies["default"].File != "{track} - {title}{ext}" {
		t.Errorf("custom template not loaded: %q", loaded.Policies["default"].File)
	}
}

func TestLoadRejectsBrokenDefault(t *testing.T) {
	root := t.TempDir()
	body := "default: missing\npolicies:\n  other:\n    name: other\n"
	if err := os.WriteFile(filepath.Join(root, FileName), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(root); err == nil {
		t.Errorf("load with missing default policy: got nil error, want failure")
	}
}

func TestHashTracksStableAndOrderIndependent(t *testing.T) {
	a := map[string][]string{"TITLE": {"A"}, "ARTIST": {"X", "Y"}}
	b := map[string][]string{"TITLE": {"B"}}
	h1 := HashTracks([]string{"01 a.flac", "02 b.flac"}, []map[string][]string{a, b})
	h2 := HashTracks([]string{"02 b.flac", "01 a.flac"}, []map[string][]string{b, a})
	if h1 != h2 {
		t.Errorf("hash changed with argument order: %s vs %s", h1, h2)
	}
	if !strings.HasPrefix(h1, "sha256:") {
		t.Errorf("hash prefix: %q", h1)
	}
	b2 := map[string][]string{"TITLE": {"changed"}}
	if HashTracks([]string{"01 a.flac", "02 b.flac"}, []map[string][]string{a, b2}) == h1 {
		t.Errorf("hash did not change when a payload changed")
	}
}
