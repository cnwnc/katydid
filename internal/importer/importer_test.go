package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"doppel.moe/katydid/internal/library"
	"doppel.moe/katydid/internal/mb"
	"doppel.moe/katydid/internal/mb/mbtest"
	"doppel.moe/katydid/internal/sidecar"
	"doppel.moe/katydid/internal/tags"
	"doppel.moe/katydid/internal/testaudio"
	"go.senan.xyz/taglib"
)

func searchBody(releases ...mb.SearchRelease) string {
	out, _ := json.Marshal(struct {
		Releases []mb.SearchRelease `json:"releases"`
	}{Releases: releases})
	return string(out)
}

func lookupBody(release mb.Release) string {
	out, _ := json.Marshal(release)
	return string(out)
}

type mbFixture struct {
	search   []mb.SearchRelease
	releases []mb.Release
}

var mbBase string

func startMB(t *testing.T, fixture mbFixture) {
	t.Helper()
	server := mbtest.Custom(t, func(r *http.Request) (string, int) {
		if r.URL.Path == "/ws/2/release" {
			return searchBody(fixture.search...), http.StatusOK
		}
		if id, ok := strings.CutPrefix(r.URL.Path, "/ws/2/release/"); ok {
			for _, release := range fixture.releases {
				if release.ID == id {
					return lookupBody(release), http.StatusOK
				}
			}
		}
		return `{"error": "no fixture"}`, http.StatusNotFound
	})
	mbBase = server.URL
}

func newManager(t *testing.T, root string) *Manager {
	t.Helper()
	client := mb.New(mbBase, "", false)
	client.SetRequestInterval(time.Millisecond)
	return New(library.NewIndex(root), client)
}

func twoTestFiles(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	testaudio.MakeTracked(t, src, "01 - First Song.flac", "First Song", "Test Artist", "Test Album", 1, 1, 2001)
	testaudio.MakeTracked(t, src, "02 - Second Song.flac", "Second Song", "Test Artist", "Test Album", 2, 1, 2001)
	return src
}

func autoFixture() mbFixture {
	return mbFixture{
		search: []mb.SearchRelease{mbtest.SyntheticSearch("rel-1", "rg-1", "Test Album", "Test Artist", "2001-10-01", 2)},
		releases: []mb.Release{
			mbtest.SyntheticRelease("rel-1", "rg-1", "Test Album", "Test Artist", "2001-10-01",
				mb.ReleaseMedia{Position: 1, Format: "CD", TrackCount: 2, Tracks: []mb.ReleaseTrack{
					mbtest.Track(1, "First Song"), mbtest.Track(2, "Second Song"),
				}}),
		},
	}
}

func TestImportAuto(t *testing.T) {
	startMB(t, autoFixture())
	root := t.TempDir()
	src := twoTestFiles(t)
	manager := newManager(t, root)

	result, err := manager.Import(context.Background(), Request{Dir: src, By: "test", Request: "add Test Album by Test Artist"})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Status != statusImported {
		t.Fatalf("status: got %q, want imported (%+v)", result.Status, result)
	}
	wantID := filepath.Join("Test Artist", "2001 - Test Album")
	if result.AlbumID != wantID {
		t.Errorf("album id: got %q, want %q", result.AlbumID, wantID)
	}

	sc, err := sidecar.Load(filepath.Join(root, wantID))
	if err != nil {
		t.Fatalf("sidecar: %v", err)
	}
	if sc.Album != "Test Album" || sc.AlbumArtist != "Test Artist" || sc.Year != 2001 {
		t.Errorf("sidecar meta: %s / %s / %d", sc.AlbumArtist, sc.Album, sc.Year)
	}
	if sc.MusicBrainz.ReleaseID != "rel-1" || sc.MusicBrainz.ReleaseGroupID != "rg-1" {
		t.Errorf("sidecar mb ids: %+v", sc.MusicBrainz)
	}
	if len(sc.Tracks) != 2 || sc.Tracks[0].Title != "First Song" || sc.Tracks[0].Track != 1 || sc.Tracks[0].Disc != 1 {
		t.Errorf("sidecar tracks: %+v", sc.Tracks)
	}
	if sc.Provenance.By != "test" || sc.Provenance.Request != "add Test Album by Test Artist" || sc.Provenance.Origin.Path != src {
		t.Errorf("provenance: %+v", sc.Provenance)
	}

	index := library.NewIndex(root)
	if err := index.Scan(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	albums := index.Albums(library.Query{})
	if len(albums) != 1 || albums[0].Pending || albums[0].Meta.Album != "Test Album" {
		t.Errorf("index after import: %+v", albums)
	}

	if _, err := os.Stat(filepath.Join(src, "01 - First Song.flac")); err != nil {
		t.Errorf("source file removed: %v", err)
	}
}

func TestImportNeedsDecisionPickAndSkip(t *testing.T) {
	startMB(t, mbFixture{
		search: []mb.SearchRelease{
			mbtest.SyntheticSearch("rel-1", "rg-1", "Test Album", "Other Artist", "2001", 5),
			mbtest.SyntheticSearch("rel-2", "rg-2", "Test Album", "Test Artist", "2019-03-03", 3),
		},
		releases: []mb.Release{
			mbtest.SyntheticRelease("rel-1", "rg-1", "Test Album", "Other Artist", "2001",
				mb.ReleaseMedia{Position: 1, TrackCount: 5}),
			// the right release carries a mismatched year AND an extra
			// track, so neither auto nor the perfect-list rule applies
			mbtest.SyntheticRelease("rel-2", "rg-2", "Test Album", "Test Artist", "2019-03-03",
				mb.ReleaseMedia{Position: 1, Format: "CD", TrackCount: 3, Tracks: []mb.ReleaseTrack{
					mbtest.Track(1, "First Song"), mbtest.Track(2, "Second Song"), mbtest.Track(3, "Third Song"),
				}}),
		},
	})
	root := t.TempDir()
	src := twoTestFiles(t)
	manager := newManager(t, root)
	ctx := context.Background()

	result, err := manager.Import(ctx, Request{Dir: src})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Status != statusNeedsDecision {
		t.Fatalf("status: got %q, want needs_decision", result.Status)
	}
	decision := result.Decision
	if decision.Token == "" || len(decision.Candidates) == 0 {
		t.Fatalf("decision: %+v", decision)
	}
	if decision.Candidates[0].ReleaseID != "rel-2" {
		t.Errorf("top candidate: got %s, want rel-2 (artist matches)", decision.Candidates[0].ReleaseID)
	}
	if len(manager.Decisions()) != 1 {
		t.Errorf("decisions list: got %d, want 1", len(manager.Decisions()))
	}

	if _, err := manager.Decide(ctx, decision.Token, DecideInput{Pick: 99}); err == nil {
		t.Errorf("pick out of range: got nil error, want failure")
	}
	if _, err := manager.Decide(ctx, "nope", DecideInput{Pick: 1}); err == nil {
		t.Errorf("unknown token: got nil error, want failure")
	}

	picked, err := manager.Decide(ctx, decision.Token, DecideInput{Pick: 1})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if picked.Status != statusImported || picked.AlbumID != filepath.Join("Test Artist", "2019 - Test Album") {
		t.Errorf("picked: %+v", picked)
	}
	if len(manager.Decisions()) != 0 {
		t.Errorf("decisions after pick: got %d, want 0", len(manager.Decisions()))
	}

	result, err = manager.Import(ctx, Request{Dir: src})
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if result.Status != statusNeedsDecision {
		t.Fatalf("second status: got %q, want needs_decision", result.Status)
	}
	skipped, err := manager.Decide(ctx, result.Decision.Token, DecideInput{Skip: true})
	if err != nil {
		t.Fatalf("skip: %v", err)
	}
	if skipped.Status != statusSkipped {
		t.Errorf("skip status: got %q, want skipped", skipped.Status)
	}
	if len(manager.Decisions()) != 0 {
		t.Errorf("decisions after skip: got %d, want 0", len(manager.Decisions()))
	}
}

func TestImportReplaceConflict(t *testing.T) {
	startMB(t, autoFixture())
	root := t.TempDir()
	src := twoTestFiles(t)
	manager := newManager(t, root)
	ctx := context.Background()

	if _, err := manager.Import(ctx, Request{Dir: src}); err != nil {
		t.Fatalf("first import: %v", err)
	}
	_, err := manager.Import(ctx, Request{Dir: src})
	if !errors.Is(err, ErrTargetExists) {
		t.Fatalf("second import: got %v, want ErrTargetExists", err)
	}

	result, err := manager.Import(ctx, Request{Dir: src, Replace: true})
	if err != nil {
		t.Fatalf("replace import: %v", err)
	}
	if result.Status != statusImported {
		t.Fatalf("replace status: %q", result.Status)
	}
	entries, err := os.ReadDir(filepath.Join(root, ".trash"))
	if err != nil || len(entries) != 1 {
		t.Errorf("trash: err=%v entries=%d", err, len(entries))
	}
}

func TestImportMappingMultiDisc(t *testing.T) {
	startMB(t, mbFixture{
		search: []mb.SearchRelease{mbtest.SyntheticSearch("rel-md", "rg-md", "Test Album", "Test Artist", "2001", 6)},
		releases: []mb.Release{
			mbtest.SyntheticRelease("rel-md", "rg-md", "Test Album", "Test Artist", "2001",
				mb.ReleaseMedia{Position: 1, Format: "CD", TrackCount: 3, Tracks: []mb.ReleaseTrack{
					mbtest.Track(1, "Alpha"), mbtest.Track(2, "Beta"), mbtest.Track(3, "Gamma"),
				}},
				mb.ReleaseMedia{Position: 2, Format: "CD", TrackCount: 3, Tracks: []mb.ReleaseTrack{
					mbtest.Track(1, "Delta"), mbtest.Track(2, "Epsilon"), mbtest.Track(3, "Zeta"),
				}}),
		},
	})

	src := t.TempDir()
	titles := []string{"Alpha", "Beta", "Gamma", "Delta", "Epsilon", "Zeta"}
	for i, title := range titles {
		testaudio.MakeTracked(t, src, fmt.Sprintf("%02d - %s.flac", i+1, title), title, "Test Artist", "Test Album", i+1, 1, 2001)
	}

	manager := newManager(t, t.TempDir())
	result, err := manager.Import(context.Background(), Request{Dir: src})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Status != statusImported {
		t.Fatalf("status: %q", result.Status)
	}

	sc, err := sidecar.Load(filepath.Join(manager.index.Root(), result.AlbumID))
	if err != nil {
		t.Fatalf("sidecar: %v", err)
	}
	if len(sc.Tracks) != 6 {
		t.Fatalf("tracks: got %d, want 6", len(sc.Tracks))
	}
	if sc.Tracks[3].Title != "Delta" || sc.Tracks[3].Disc != 2 || sc.Tracks[3].Track != 1 {
		t.Errorf("track 4 mapping: %+v", sc.Tracks[3])
	}
	if sc.Tracks[0].Disc != 1 || sc.Tracks[0].Track != 1 {
		t.Errorf("track 1 mapping: %+v", sc.Tracks[0])
	}
}

func TestImportMappingThroughDecisionWhenCountsDiffer(t *testing.T) {
	startMB(t, mbFixture{
		search: []mb.SearchRelease{mbtest.SyntheticSearch("rel-bonus", "rg-bonus", "Test Album", "Test Artist", "2001", 4)},
		releases: []mb.Release{
			mbtest.SyntheticRelease("rel-bonus", "rg-bonus", "Test Album", "Test Artist", "2001",
				mb.ReleaseMedia{Position: 1, Format: "CD", TrackCount: 4, Tracks: []mb.ReleaseTrack{
					mbtest.Track(1, "First Song (Remaster)"), mbtest.Track(2, "Second Song"),
					mbtest.Track(3, "Bonus Track"), mbtest.Track(4, "Live Take"),
				}}),
		},
	})
	root := t.TempDir()
	src := twoTestFiles(t)
	manager := newManager(t, root)
	ctx := context.Background()

	result, err := manager.Import(ctx, Request{Dir: src})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Status != statusNeedsDecision {
		t.Fatalf("status: got %q, want needs_decision (2 files vs 4 release tracks)", result.Status)
	}

	picked, err := manager.Decide(ctx, result.Decision.Token, DecideInput{Pick: 1})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if picked.Status != statusImported {
		t.Fatalf("picked status: %q", picked.Status)
	}

	sc, err := sidecar.Load(filepath.Join(root, picked.AlbumID))
	if err != nil {
		t.Fatalf("sidecar: %v", err)
	}
	if len(sc.Tracks) != 2 {
		t.Fatalf("tracks: got %d, want 2", len(sc.Tracks))
	}
	if sc.Tracks[0].Title != "First Song (Remaster)" || sc.Tracks[0].Track != 1 {
		t.Errorf("key-mapped track 1: %+v", sc.Tracks[0])
	}
	if sc.Tracks[1].Title != "Second Song" || sc.Tracks[1].Track != 2 {
		t.Errorf("key-mapped track 2: %+v", sc.Tracks[1])
	}
}

func TestImportNoCandidates(t *testing.T) {
	startMB(t, mbFixture{})
	manager := newManager(t, t.TempDir())
	src := twoTestFiles(t)
	_, err := manager.Import(context.Background(), Request{Dir: src})
	if err == nil || !strings.Contains(err.Error(), "no musicbrainz candidates") {
		t.Errorf("import with no candidates: got %v, want explicit failure", err)
	}
}

func TestImportMissingDir(t *testing.T) {
	startMB(t, autoFixture())
	manager := newManager(t, t.TempDir())
	_, err := manager.Import(context.Background(), Request{Dir: "/nonexistent-dir-xyz"})
	if err == nil {
		t.Errorf("import missing dir: got nil error, want failure")
	}
}

func TestImportAppliesPolicy(t *testing.T) {
	startMB(t, autoFixture())
	root := t.TempDir()
	src := twoTestFiles(t)
	manager := newManager(t, root)

	result, err := manager.Import(context.Background(), Request{Dir: src, By: "test"})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Status != statusImported {
		t.Fatalf("status: %q", result.Status)
	}

	albumDir := filepath.Join(root, result.AlbumID)
	entries, err := os.ReadDir(albumDir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if len(names) != 3 || names[0] != "01 - First Song.flac" || names[1] != "02 - Second Song.flac" || names[2] != "album.yaml" {
		t.Errorf("album dir contents after policy rename: %v", names)
	}

	raw, err := taglib.ReadTags(filepath.Join(albumDir, "01 - First Song.flac"))
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	if got := raw["TITLE"]; len(got) != 1 || got[0] != "First Song" {
		t.Errorf("written title: %v", got)
	}
	if got := raw["ARTIST"]; len(got) != 1 || got[0] != "Test Artist" {
		t.Errorf("written artist: %v", got)
	}
	if got := raw["TRACKNUMBER"]; len(got) != 1 || got[0] != "1" {
		t.Errorf("written tracknumber: %v", got)
	}
	if got := raw["TRACKTOTAL"]; len(got) != 1 || got[0] != "2" {
		t.Errorf("written tracktotal: %v", got)
	}
	if got := raw["MUSICBRAINZ_TRACKID"]; len(got) != 1 || got[0] != "rec-First Song" {
		t.Errorf("written mb recording id: %v", got)
	}
	if got := raw["MUSICBRAINZ_RELEASETRACKID"]; len(got) != 1 || got[0] != "trk-First Song" {
		t.Errorf("written mb release track id: %v", got)
	}
	if got := raw["MUSICBRAINZ_ALBUMID"]; len(got) != 1 || got[0] != "rel-1" {
		t.Errorf("written mb album id: %v", got)
	}
	if got := raw["DATE"]; len(got) != 1 || got[0] != "2001-10-01" {
		t.Errorf("written date: %v", got)
	}

	sc, err := sidecar.Load(albumDir)
	if err != nil {
		t.Fatalf("sidecar: %v", err)
	}
	if sc.TagState == nil || sc.TagState.Policy != "default" || !strings.HasPrefix(sc.TagState.StateHash, "sha256:") {
		t.Fatalf("tag state: %+v", sc.TagState)
	}
	if sc.Tracks[0].Tags == nil || len(sc.Tracks[0].Tags["TITLE"]) != 1 {
		t.Errorf("sidecar payload missing: %+v", sc.Tracks[0])
	}
	if _, err := manager.index.Album(result.AlbumID); err != nil {
		t.Errorf("album not in index: %v", err)
	}
}

func TestImportScrubbsUnmanagedTags(t *testing.T) {
	startMB(t, autoFixture())
	src := twoTestFiles(t)
	first := filepath.Join(src, "01 - First Song.flac")
	if err := taglib.WriteTags(first, map[string][]string{"COMMENT": {"ripped by someone"}}, 0); err != nil {
		t.Fatalf("inject comment: %v", err)
	}
	manager := newManager(t, t.TempDir())

	result, err := manager.Import(context.Background(), Request{Dir: src})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	raw, err := taglib.ReadTags(filepath.Join(libRoot(manager), result.AlbumID, "01 - First Song.flac"))
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	if got, ok := raw["COMMENT"]; ok {
		t.Errorf("comment survived import: %v", got)
	}
}

func TestCheckDetectsDriftAfterExternalEdit(t *testing.T) {
	startMB(t, autoFixture())
	root := t.TempDir()
	src := twoTestFiles(t)
	manager := newManager(t, root)
	result, err := manager.Import(context.Background(), Request{Dir: src})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if findings := manager.index.Check(); len(findings) != 0 {
		t.Fatalf("fresh import has findings: %+v", findings)
	}

	edited := filepath.Join(libRoot(manager), result.AlbumID, "01 - First Song.flac")
	if err := taglib.WriteTags(edited, map[string][]string{"TITLE": {"Edited By Hand"}}, 0); err != nil {
		t.Fatalf("edit tags: %v", err)
	}
	if err := manager.index.Scan(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	drifted := false
	for _, finding := range manager.index.Check() {
		if finding.Kind == library.KindTagDrift && strings.Contains(finding.Detail, "TITLE") {
			drifted = true
		}
	}
	if !drifted {
		t.Errorf("expected tag drift after external edit: %+v", manager.index.Check())
	}
}

func TestRetagRestoresPolicyAndNames(t *testing.T) {
	startMB(t, autoFixture())
	root := t.TempDir()
	src := twoTestFiles(t)
	manager := newManager(t, root)
	result, err := manager.Import(context.Background(), Request{Dir: src})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	albumID := result.AlbumID

	edited := filepath.Join(libRoot(manager), albumID, "01 - First Song.flac")
	if err := taglib.WriteTags(edited, map[string][]string{"TITLE": {"Edited By Hand"}}, 0); err != nil {
		t.Fatalf("edit tags: %v", err)
	}

	retag := manager.Retag(albumID, "")
	if retag.Error != "" {
		t.Fatalf("retag: %s", retag.Error)
	}

	raw, err := taglib.ReadTags(filepath.Join(libRoot(manager), albumID, "01 - First Song.flac"))
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	if got := raw["TITLE"]; len(got) != 1 || got[0] != "First Song" {
		t.Errorf("title after retag: %v", got)
	}
	if findings := manager.index.Check(); len(findings) != 0 {
		t.Errorf("findings after retag: %+v", findings)
	}

	sc, err := sidecar.Load(filepath.Join(libRoot(manager), albumID))
	if err != nil {
		t.Fatalf("sidecar: %v", err)
	}
	if sc.TagState == nil || sc.Tracks[0].Tags == nil {
		t.Errorf("retag did not record tag state: %+v", sc.TagState)
	}
}

func TestRetagAllSkipsPending(t *testing.T) {
	startMB(t, autoFixture())
	manager := newManager(t, t.TempDir())

	// a pending album: audio files without sidecar, never imported
	pendingDir := filepath.Join(manager.index.Root(), "Some Artist", "2020 - Some Album")
	if err := os.MkdirAll(pendingDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	testaudio.MakeTracked(t, pendingDir, "01 - Pending Song.flac", "Pending Song", "Some Artist", "Some Album", 1, 1, 2020)
	if err := manager.index.Scan(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	src2 := twoTestFiles(t)
	if _, err := manager.Import(context.Background(), Request{Dir: src2}); err != nil {
		t.Fatalf("import: %v", err)
	}

	results := manager.RetagAll("")
	if len(results) != 2 {
		t.Fatalf("results: %+v", results)
	}
	byID := map[string]RetagResult{}
	for _, r := range results {
		byID[r.AlbumID] = r
	}
	pending, ok := byID[filepath.Join("Some Artist", "2020 - Some Album")]
	if !ok || pending.Error == "" {
		t.Errorf("pending album should be skipped with error: %+v", results)
	}
	for _, r := range results {
		if r.Error == "" && r.AlbumID != filepath.Join("Some Artist", "2020 - Some Album") {
			if r.AlbumID == "" {
				t.Errorf("empty album id in results: %+v", results)
			}
		}
	}
}

func libRoot(m *Manager) string { return m.index.Root() }

func TestImportVariousArtists(t *testing.T) {
	release := mbtest.SyntheticRelease("rel-va", "rg-va", "Test Split", "Various Artists", "2003-05-01",
		mb.ReleaseMedia{Position: 1, Format: "CD", TrackCount: 2, Tracks: []mb.ReleaseTrack{
			creditTrack(1, "Song One", "Band A"),
			creditFeaturingTrack(2, "Song Two", "Band B", "Band C"),
		}})
	release.ReleaseGroup.SecondaryTypes = []string{"Compilation"}
	startMB(t, mbFixture{
		search:   []mb.SearchRelease{mbtest.SyntheticSearch("rel-va", "rg-va", "Test Split", "Various Artists", "2003-05-01", 2)},
		releases: []mb.Release{release},
	})

	src := t.TempDir()
	testaudio.MakeTrackedAlbum(t, src, "01 - Song One.flac", "Song One", "Band A", "Various Artists", "Test Split", 1, 1, 2003)
	testaudio.MakeTrackedAlbum(t, src, "02 - Song Two.flac", "Song Two", "Band B feat. Band C", "Various Artists", "Test Split", 2, 1, 2003)

	manager := newManager(t, t.TempDir())
	result, err := manager.Import(context.Background(), Request{Dir: src, By: "test"})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Status != statusImported {
		t.Fatalf("status: %q (%+v)", result.Status, result)
	}
	if !strings.HasPrefix(result.AlbumID, "Various Artists/2003") {
		t.Fatalf("album id: %q", result.AlbumID)
	}
	albumDir := filepath.Join(manager.index.Root(), result.AlbumID)

	rawOne, err := taglib.ReadTags(filepath.Join(albumDir, "01 - Song One.flac"))
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	if got := rawOne["COMPILATION"]; len(got) != 1 || got[0] != "1" {
		t.Errorf("compilation flag: %v", got)
	}
	if got := rawOne["ALBUMARTIST"]; len(got) != 1 || got[0] != "Various Artists" {
		t.Errorf("albumartist: %v", got)
	}
	if got := rawOne["ARTIST"]; len(got) != 1 || got[0] != "Band A" {
		t.Errorf("track artist (mb credit wins over file tags): %v", got)
	}
	if got := rawOne["ARTISTS"]; len(got) != 1 || got[0] != "Band A" {
		t.Errorf("track artists list: %v", got)
	}
	if got := rawOne["RELEASETYPE"]; len(got) != 1 || got[0] != "Album, Compilation" {
		t.Errorf("releasetype: %v", got)
	}

	rawTwo, err := taglib.ReadTags(filepath.Join(albumDir, "02 - Song Two.flac"))
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	if got := rawTwo["ARTIST"]; len(got) != 1 || got[0] != "Band B" {
		t.Errorf("feat split on track credit: %v", got)
	}
	if got := rawTwo["ARTISTS"]; len(got) != 2 || got[0] != "Band B" || got[1] != "Band C" {
		t.Errorf("feat artists preserved in plural: %v", got)
	}

	sc, err := sidecar.Load(albumDir)
	if err != nil {
		t.Fatalf("sidecar: %v", err)
	}
	if !sc.Compilation {
		t.Errorf("sidecar compilation flag unset")
	}
	if got := sc.Tracks[1].Artist; got != "Band B feat. Band C" {
		t.Errorf("sidecar display credit: %q", got)
	}
}

func creditTrack(position int, title, artist string) mb.ReleaseTrack {
	track := mbtest.Track(position, title)
	track.ArtistCredit = []mb.ArtistCredit{{Name: artist}}
	return track
}

func creditFeaturingTrack(position int, title, main, featured string) mb.ReleaseTrack {
	track := mbtest.Track(position, title)
	track.ArtistCredit = []mb.ArtistCredit{
		{Name: main, Joinphrase: " feat. "},
		{Name: featured},
	}
	return track
}

func TestImportMBIDOverride(t *testing.T) {
	startMB(t, mbFixture{
		search: []mb.SearchRelease{},
		releases: []mb.Release{
			mbtest.SyntheticRelease("rel-manual", "rg-manual", "Test Album", "Test Artist", "2001-10-01",
				mb.ReleaseMedia{Position: 1, Format: "CD", TrackCount: 2, Tracks: []mb.ReleaseTrack{
					mbtest.Track(1, "First Song"), mbtest.Track(2, "Second Song"),
				}}),
		},
	})
	root := t.TempDir()
	src := twoTestFiles(t)
	manager := newManager(t, root)

	result, err := manager.Import(context.Background(), Request{Dir: src, MBID: "rel-manual", By: "test"})
	if err != nil {
		t.Fatalf("import by mbid: %v", err)
	}
	if result.Status != statusImported {
		t.Fatalf("status: got %q, want imported (%+v)", result.Status, result)
	}
	wantID := filepath.Join("Test Artist", "2001 - Test Album")
	if result.AlbumID != wantID {
		t.Errorf("album id: got %q, want %q", result.AlbumID, wantID)
	}
	sc, err := sidecar.Load(filepath.Join(root, wantID))
	if err != nil {
		t.Fatalf("sidecar: %v", err)
	}
	if sc.MusicBrainz.ReleaseID != "rel-manual" || sc.MusicBrainz.ReleaseGroupID != "rg-manual" {
		t.Errorf("sidecar mb ids: %+v", sc.MusicBrainz)
	}

	if _, err := manager.Import(context.Background(), Request{Dir: src, MBID: "missing-id"}); err == nil {
		t.Errorf("unknown mbid should fail loud")
	}
}

func TestImportRejectsUnmatchedFiles(t *testing.T) {
	startMB(t, mbFixture{
		search: []mb.SearchRelease{mbtest.SyntheticSearch("rel-4", "rg-4", "Test Album", "Test Artist", "2001-10-01", 4)},
		releases: []mb.Release{
			mbtest.SyntheticRelease("rel-4", "rg-4", "Test Album", "Test Artist", "2001-10-01",
				mb.ReleaseMedia{Position: 1, Format: "CD", TrackCount: 4, Tracks: []mb.ReleaseTrack{
					mbtest.Track(1, "First Song"), mbtest.Track(2, "Second Song"),
					mbtest.Track(3, "Third Song"), mbtest.Track(4, "Fourth Song"),
				}}),
		},
	})
	root := t.TempDir()
	src := t.TempDir()
	for _, spec := range []struct {
		base, title string
		track       int
	}{{"01 - First Song.flac", "First Song", 1}, {"02 - Second Song.flac", "Second Song", 2}, {"03 - Third Song.flac", "Third Song", 3}, {"04 - Fourth Song.flac", "Fourth Song", 4}, {"05 - Outtake.flac", "Outtake", 5}} {
		testaudio.MakeTracked(t, src, spec.base, spec.title, "Test Artist", "Test Album", spec.track, 1, 2001)
	}
	manager := newManager(t, root)

	result, err := manager.Import(context.Background(), Request{Dir: src})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Status != statusNeedsDecision {
		t.Fatalf("status: got %q, want needs_decision (%+v)", result.Status, result)
	}
	if len(result.Notes) == 0 || !strings.Contains(result.Notes[0], "rejected") {
		t.Errorf("notes should explain the rejection: %+v", result.Notes)
	}

	token := result.Decision.Token
	reparked, err := manager.Decide(context.Background(), token, DecideInput{Pick: 1})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if reparked.Status != statusNeedsDecision || reparked.Decision.Token != token {
		t.Fatalf("decide should re-park with the same token: %+v", reparked)
	}
	if reparked.Decision.Remap == nil {
		t.Fatalf("re-park should carry a remap table: %+v", reparked.Decision)
	}
	if len(reparked.Decision.Remap.Files) != 5 || len(reparked.Decision.Remap.Tracks) != 4 {
		t.Fatalf("remap table: %d files, %d tracks", len(reparked.Decision.Remap.Files), len(reparked.Decision.Remap.Tracks))
	}
	if _, unassigned := reparked.Decision.Remap.Map[5]; unassigned {
		t.Fatalf("file 5 should be unpaired: %+v", reparked.Decision.Remap.Map)
	}

	accepted, err := manager.Decide(context.Background(), token, DecideInput{Accept: true})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if accepted.Status != statusImported || accepted.AlbumID == "" {
		t.Fatalf("accept: %+v err %v", accepted, err)
	}
	dropNote := false
	for _, note := range accepted.Notes {
		if strings.Contains(note, "left out 1 file(s)") {
			dropNote = true
		}
	}
	if !dropNote {
		t.Errorf("accept should note the dropped file: %+v", accepted.Notes)
	}
	sc, err := sidecar.Load(filepath.Join(root, accepted.AlbumID))
	if err != nil {
		t.Fatalf("sidecar: %v", err)
	}
	if len(sc.Tracks) != 4 {
		t.Errorf("published %d tracks, want 4 (outtake left out)", len(sc.Tracks))
	}
}

func TestPairFilesRejectsShiftedList(t *testing.T) {
	// a release in the group dropped the album's first track, so equal
	// counts used to pair every remaining file one position off and
	// retitle the whole album
	release := mbtest.SyntheticRelease("rel-shift", "rg-shift", "Test Album", "Test Artist", "2022-12-07",
		mb.ReleaseMedia{Position: 1, Format: "Digital Media", TrackCount: 3, Tracks: []mb.ReleaseTrack{
			mbtest.Track(1, "Descent"), mbtest.Track(2, "Pink!"), mbtest.Track(3, "Closer"),
		}})
	files := []sourceFile{
		{base: "01 - Warning.flac", tags: tags.Tags{Title: "Warning", TrackNumber: 1, DiscNumber: 1}},
		{base: "02 - Descent.flac", tags: tags.Tags{Title: "Descent", TrackNumber: 2, DiscNumber: 1}},
		{base: "03 - Pink!.flac", tags: tags.Tags{Title: "Pink!", TrackNumber: 3, DiscNumber: 1}},
	}

	pairing, unpaired, err := pairFiles(orderFiles(files), &release, nil)
	if err != nil {
		t.Fatalf("pairFiles: %v", err)
	}
	if len(unpaired) != 1 || unpaired[0] != 1 {
		t.Fatalf("unpaired: %v, want [1]", unpaired)
	}
	if pairing[2] != 1 || pairing[3] != 2 {
		t.Fatalf("pairing: %v, want 2->1 and 3->2 by title", pairing)
	}
	if _, ok := pairing[1]; ok {
		t.Fatalf("file 1 paired despite a shifted track list: %v", pairing)
	}
}

func TestPairFilesRejectsDisagreeingTitles(t *testing.T) {
	release := mbtest.SyntheticRelease("rel-dis", "rg-dis", "Test Album", "Test Artist", "2001-10-01",
		mb.ReleaseMedia{Position: 1, Format: "CD", TrackCount: 3, Tracks: []mb.ReleaseTrack{
			mbtest.Track(1, "Rocks"), mbtest.Track(2, "Sand"), mbtest.Track(3, "Wind"),
		}})
	files := []sourceFile{
		{base: "01 - Sun.flac", tags: tags.Tags{Title: "Sun", TrackNumber: 1, DiscNumber: 1}},
		{base: "02 - Moon.flac", tags: tags.Tags{Title: "Moon", TrackNumber: 2, DiscNumber: 1}},
		{base: "03 - Star.flac", tags: tags.Tags{Title: "Star", TrackNumber: 3, DiscNumber: 1}},
	}

	pairing, unpaired, err := pairFiles(orderFiles(files), &release, nil)
	if err != nil {
		t.Fatalf("pairFiles: %v", err)
	}
	if len(pairing) != 0 || len(unpaired) != 3 {
		t.Fatalf("pairing %v unpaired %v, want nothing paired", pairing, unpaired)
	}
}

func TestPairFilesEqualCountsAgreeingTitles(t *testing.T) {
	release := mbtest.SyntheticRelease("rel-eq", "rg-eq", "Test Album", "Test Artist", "2001-10-01",
		mb.ReleaseMedia{Position: 1, Format: "CD", TrackCount: 2, Tracks: []mb.ReleaseTrack{
			mbtest.Track(1, "First Song"), mbtest.Track(2, "Second Song"),
		}})
	files := []sourceFile{
		{base: "01 - First Song.flac", tags: tags.Tags{Title: "First Song", TrackNumber: 1, DiscNumber: 1}},
		{base: "02 - Second Song.flac", tags: tags.Tags{Title: "Second Song", TrackNumber: 2, DiscNumber: 1}},
	}

	pairing, unpaired, err := pairFiles(orderFiles(files), &release, nil)
	if err != nil {
		t.Fatalf("pairFiles: %v", err)
	}
	if len(unpaired) != 0 || pairing[1] != 1 || pairing[2] != 2 {
		t.Fatalf("pairing %v unpaired %v, want 1->1 and 2->2", pairing, unpaired)
	}
}

func TestPairFilesTrustsNumbersAcrossScripts(t *testing.T) {
	release := mbtest.SyntheticRelease("rel-jp", "rg-jp", "Test Album", "Test Artist", "2001-10-01",
		mb.ReleaseMedia{Position: 1, Format: "CD", TrackCount: 2, Tracks: []mb.ReleaseTrack{
			mbtest.Track(1, "最初の歌"), mbtest.Track(2, "二番目の歌"),
		}})
	files := []sourceFile{
		{base: "01 - First Song.flac", tags: tags.Tags{Title: "First Song", TrackNumber: 1, DiscNumber: 1}},
		{base: "02 - Second Song.flac", tags: tags.Tags{Title: "Second Song", TrackNumber: 2, DiscNumber: 1}},
	}

	pairing, unpaired, err := pairFiles(orderFiles(files), &release, nil)
	if err != nil {
		t.Fatalf("pairFiles: %v", err)
	}
	if len(unpaired) != 0 || pairing[1] != 1 || pairing[2] != 2 {
		t.Fatalf("pairing %v unpaired %v, want numbers trusted across scripts", pairing, unpaired)
	}
}

func TestImportNotesTitleChanges(t *testing.T) {
	startMB(t, mbFixture{
		search: []mb.SearchRelease{mbtest.SyntheticSearch("rel-5", "rg-5", "Test Album", "Test Artist", "2001-10-01", 2)},
		releases: []mb.Release{
			mbtest.SyntheticRelease("rel-5", "rg-5", "Test Album", "Test Artist", "2001-10-01",
				mb.ReleaseMedia{Position: 1, Format: "CD", TrackCount: 2, Tracks: []mb.ReleaseTrack{
					mbtest.Track(1, "最初の歌"), mbtest.Track(2, "二番目の歌"),
				}}),
		},
	})
	root := t.TempDir()
	src := t.TempDir()
	testaudio.MakeTracked(t, src, "01 - First Song.flac", "First Song", "Test Artist", "Test Album", 1, 1, 2001)
	testaudio.MakeTracked(t, src, "02 - Second Song.flac", "Second Song", "Test Artist", "Test Album", 2, 1, 2001)
	testaudio.MakeTracked(t, src, "03 - Outtake.flac", "Outtake", "Test Artist", "Test Album", 3, 1, 2001)
	manager := newManager(t, root)

	result, err := manager.Import(context.Background(), Request{Dir: src})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Status != statusNeedsDecision {
		t.Fatalf("status: got %q, want needs_decision (%+v)", result.Status, result)
	}
	token := result.Decision.Token
	parked, err := manager.Decide(context.Background(), token, DecideInput{Pick: 1})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if parked.Status != statusNeedsDecision || parked.Decision == nil || parked.Decision.Remap == nil {
		t.Fatalf("pick should re-park with a remap table: %+v", parked)
	}
	accepted, err := manager.Decide(context.Background(), token, DecideInput{Accept: true})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if accepted.Status != statusImported {
		t.Fatalf("accept: %+v", accepted)
	}

	for _, want := range []string{"title: First Song -> 最初の歌", "title: Second Song -> 二番目の歌"} {
		found := false
		for _, note := range accepted.Notes {
			if note == want {
				found = true
			}
		}
		if !found {
			t.Errorf("notes should carry %q: %+v", want, accepted.Notes)
		}
	}

	leftOutIndex := -1
	titleIndexes := []int{}
	for i, note := range accepted.Notes {
		if strings.Contains(note, "left out 1 file(s)") {
			leftOutIndex = i
		}
		if strings.HasPrefix(note, "title: ") {
			titleIndexes = append(titleIndexes, i)
		}
	}
	if leftOutIndex < 0 {
		t.Errorf("notes should carry the left out note: %+v", accepted.Notes)
	}
	if len(titleIndexes) != 2 {
		t.Errorf("expected 2 title notes: %+v", accepted.Notes)
	}
	for _, i := range titleIndexes {
		if i < leftOutIndex {
			t.Errorf("title notes should follow the left out note: %+v", accepted.Notes)
		}
	}

	sc, err := sidecar.Load(filepath.Join(root, accepted.AlbumID))
	if err != nil {
		t.Fatalf("sidecar: %v", err)
	}
	if len(sc.Tracks) != 2 || sc.Tracks[0].Title != "最初の歌" {
		t.Errorf("published tracks: %+v", sc.Tracks)
	}
}

func TestImportRemapPairsMismatchedFiles(t *testing.T) {
	startMB(t, mbFixture{
		search: []mb.SearchRelease{mbtest.SyntheticSearch("rel-2", "rg-2", "An Awesome Wave", "alt-J", "2012-05-25", 2)},
		releases: []mb.Release{
			mbtest.SyntheticRelease("rel-2", "rg-2", "An Awesome Wave", "alt-J", "2012-05-25",
				mb.ReleaseMedia{Position: 1, Format: "CD", TrackCount: 2, Tracks: []mb.ReleaseTrack{
					mbtest.Track(1, "Intro"), mbtest.Track(2, "Bloodflood"),
				}}),
		},
	})
	root := t.TempDir()
	src := t.TempDir()
	testaudio.MakeTracked(t, src, "01 Intro.flac", "Intro", "alt-J", "An Awesome Wave", 1, 1, 2012)
	testaudio.MakeTracked(t, src, "01 Intro.1.flac", "Intro", "alt-J", "An Awesome Wave", 1, 1, 2012)
	testaudio.MakeTracked(t, src, "02 Bloodflood.flac", "Bloodflood", "alt-J", "An Awesome Wave", 2, 1, 2012)
	manager := newManager(t, root)

	result, err := manager.Import(context.Background(), Request{Dir: src})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Status != statusNeedsDecision {
		t.Fatalf("status: got %q (%+v)", result.Status, result)
	}
	token := result.Decision.Token
	table, err := manager.Decide(context.Background(), token, DecideInput{Pick: 1})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if table.Decision == nil || table.Decision.Remap == nil {
		t.Fatalf("expected a remap table, got %+v", table)
	}
	remap := table.Decision.Remap
	if len(remap.Files) != 3 || len(remap.Tracks) != 2 {
		t.Fatalf("table: %d files, %d tracks", len(remap.Files), len(remap.Tracks))
	}
	// greedy pairs file 1 (track number) and file 3 (title); the
	// duplicate Intro.1 has nowhere to go
	if got := remap.Map[1]; got != 1 {
		t.Fatalf("file 1 should tentatively claim track 1: %+v", remap.Map)
	}
	if _, paired := remap.Map[2]; paired {
		t.Fatalf("Intro.1 should start unpaired: %+v", remap.Map)
	}
	if got := remap.Map[3]; got != 2 {
		t.Fatalf("Bloodflood should pair with track 2: %+v", remap.Map)
	}

	// the user knows Intro.1 is the real Intro: steal track 1 for file 2
	steal, err := manager.Decide(context.Background(), token, DecideInput{RemapFile: 2, RemapTrack: 1})
	if err != nil {
		t.Fatalf("remap: %v", err)
	}
	if steal.Decision == nil || steal.Decision.Remap == nil {
		t.Fatalf("after steal the table should re-print: %+v", steal)
	}
	if got := steal.Decision.Remap.Map[2]; got != 1 {
		t.Errorf("file 2 should now pair track 1: %+v", steal.Decision.Remap.Map)
	}
	if _, paired := steal.Decision.Remap.Map[1]; paired {
		t.Errorf("file 1 should be unpaired after the steal: %+v", steal.Decision.Remap.Map)
	}

	// file 1 is the alternate take: it does not belong on the release
	accepted, err := manager.Decide(context.Background(), token, DecideInput{Accept: true})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if accepted.Status != statusImported {
		t.Fatalf("accept: %+v", accepted)
	}
	sc, err := sidecar.Load(filepath.Join(root, accepted.AlbumID))
	if err != nil {
		t.Fatalf("sidecar: %v", err)
	}
	if len(sc.Tracks) != 2 || sc.Tracks[0].Title != "Intro" {
		t.Fatalf("tracks: %+v", sc.Tracks)
	}
	if sc.Tracks[0].Title != "Intro" || sc.Tracks[0].Track != 1 {
		t.Errorf("remapped track: %+v", sc.Tracks[0])
	}
}

func TestDecideRemapStealsTrack(t *testing.T) {
	startMB(t, mbFixture{
		search: []mb.SearchRelease{mbtest.SyntheticSearch("rel-2", "rg-2", "An Awesome Wave", "alt-J", "2012-05-25", 2)},
		releases: []mb.Release{
			mbtest.SyntheticRelease("rel-2", "rg-2", "An Awesome Wave", "alt-J", "2012-05-25",
				mb.ReleaseMedia{Position: 1, Format: "CD", TrackCount: 2, Tracks: []mb.ReleaseTrack{
					mbtest.Track(1, "Intro"), mbtest.Track(2, "Bloodflood"),
				}}),
		},
	})
	root := t.TempDir()
	src := t.TempDir()
	testaudio.MakeTracked(t, src, "01 Intro.flac", "Intro", "alt-J", "An Awesome Wave", 1, 1, 2012)
	testaudio.MakeTracked(t, src, "01 Intro.1.flac", "Intro", "alt-J", "An Awesome Wave", 1, 1, 2012)
	testaudio.MakeTracked(t, src, "02 Bloodflood.flac", "Bloodflood", "alt-J", "An Awesome Wave", 2, 1, 2012)
	manager := newManager(t, root)

	result, err := manager.Import(context.Background(), Request{Dir: src})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	token := result.Decision.Token
	ctx := context.Background()

	if _, err := manager.Decide(ctx, token, DecideInput{Pick: 1}); err != nil {
		t.Fatalf("pick: %v", err)
	}
	if _, err := manager.Decide(ctx, token, DecideInput{RemapFile: 1, RemapTrack: 1}); err != nil {
		t.Fatalf("first assign: %v", err)
	}
	// file 1 already claimed track 1; file 2 must supersede it, not error
	second, err := manager.Decide(ctx, token, DecideInput{RemapFile: 2, RemapTrack: 1})
	if err != nil {
		t.Fatalf("superseding assign should replace the earlier claim: %v", err)
	}
	if got := second.Decision.Remap.Map[2]; got != 1 {
		t.Errorf("file 2 should own track 1: %+v", second.Decision.Remap.Map)
	}
	if _, paired := second.Decision.Remap.Map[1]; paired {
		t.Errorf("file 1 should be unpaired after supersede: %+v", second.Decision.Remap.Map)
	}
	accepted, err := manager.Decide(ctx, token, DecideInput{Accept: true})
	if err != nil || accepted.Status != statusImported {
		t.Fatalf("accept after supersede: %+v err %v", accepted, err)
	}
	sc, err := sidecar.Load(filepath.Join(root, accepted.AlbumID))
	if err != nil {
		t.Fatalf("sidecar: %v", err)
	}
	if len(sc.Tracks) != 2 || sc.Tracks[0].Title != "Intro" {
		t.Fatalf("tracks: %+v", sc.Tracks)
	}
	// file 2 is Intro.flac per the table order, so supersede keeps
	// Intro.flac and drops the Intro.1 duplicate
	dropped := false
	for _, note := range accepted.Notes {
		if strings.Contains(note, "left out 1 file(s) not part of the release: 01 Intro.1.flac") {
			dropped = true
		}
	}
	if !dropped {
		t.Errorf("drop note should name Intro.1.flac: %+v", accepted.Notes)
	}
}

func TestResolveGroupFlow(t *testing.T) {
	startMB(t, mbFixture{
		search: []mb.SearchRelease{
			mbtest.SyntheticSearch("rg-single", "rg-single", "Test Album", "Test Artist", "2001", 2),
			mbtest.SyntheticSearch("rg-album", "rg-album", "Test Album", "Test Artist", "2001", 10),
		},
		releases: []mb.Release{
			mbtest.SyntheticRelease("rel-orig", "rg-album", "Test Album", "Test Artist", "2001",
				mb.ReleaseMedia{Position: 1, Format: "CD", TrackCount: 2, Tracks: []mb.ReleaseTrack{
					mbtest.Track(1, "First Song"), mbtest.Track(2, "Second Song"),
				}}),
		},
	})
	// group search goes through the same /ws/2/release-group path; the
	// fixture server only handles /ws/2/release, so patch the handler via
	// a dedicated server below instead
	_ = startMB
	server := mbtest.Custom(t, func(r *http.Request) (string, int) {
		if strings.HasPrefix(r.URL.Path, "/ws/2/release-group") {
			return groupSearchBody(), http.StatusOK
		}
		if strings.Contains(r.URL.RawQuery, "rgid%3Arg-album") {
			return searchBody([]mb.SearchRelease{
				mbtest.SyntheticSearch("rel-remaster", "rg-album", "Test Album", "Test Artist", "2011-01-01", 2),
				mbtest.SyntheticSearch("rel-orig", "rg-album", "Test Album", "Test Artist", "2001", 2),
			}...), http.StatusOK
		}
		if id, ok := strings.CutPrefix(r.URL.Path, "/ws/2/release/"); ok {
			for _, release := range []mb.Release{
				mbtest.SyntheticRelease("rel-orig", "rg-album", "Test Album", "Test Artist", "2001",
					mb.ReleaseMedia{Position: 1, Format: "CD", TrackCount: 2, Tracks: []mb.ReleaseTrack{
						mbtest.Track(1, "First Song"), mbtest.Track(2, "Second Song"),
					}}),
			} {
				if release.ID == id {
					return lookupBody(release), http.StatusOK
				}
			}
		}
		return `{"error": "no fixture"}`, http.StatusNotFound
	})
	mbBase = server.URL
	manager := newManager(t, t.TempDir())

	// fuzzy specifier: group candidates, album type outranks single
	groups, err := manager.Resolve(context.Background(), "Test Artist", "Test Album", 2001, "", "", 0)
	if err != nil {
		t.Fatalf("resolve groups: %v", err)
	}
	if len(groups) == 0 {
		t.Fatalf("no group candidates")
	}
	if groups[0].ReleaseID != "rg-album" {
		t.Errorf("top group = %s, want rg-album (type tiebreak)", groups[0].ReleaseID)
	}

	// group pick: releases ranked oldest first, titles carried
	releases, err := manager.Resolve(context.Background(), "Test Artist", "Test Album", 2001, "", "rg-album", 0)
	if err != nil {
		t.Fatalf("resolve group: %v", err)
	}
	if len(releases) == 0 || releases[0].ReleaseID != "rel-orig" {
		t.Fatalf("top release = %+v, want rel-orig (oldest)", releases)
	}
	if len(releases[0].TrackTitles) != 2 || releases[0].TrackTitles[0] != "First Song" {
		t.Errorf("track titles: %+v", releases[0].TrackTitles)
	}
}

func groupSearchBody() string {
	groups := []mb.SearchReleaseGroup{
		{ID: "rg-single", Score: 100, Title: "Test Album", FirstReleaseDate: "2001", PrimaryType: "Single",
			ArtistCredit: []mb.ArtistCredit{{Name: "Test Artist"}}},
		{ID: "rg-album", Score: 100, Title: "Test Album", FirstReleaseDate: "2001", PrimaryType: "Album",
			ArtistCredit: []mb.ArtistCredit{{Name: "Test Artist"}}},
	}
	data, _ := json.Marshal(map[string]any{"release-groups": groups})
	return string(data)
}

func TestResolveLimit(t *testing.T) {
	server := mbtest.Custom(t, func(r *http.Request) (string, int) {
		if strings.HasPrefix(r.URL.Path, "/ws/2/release-group") {
			return manyGroupsBody(7), http.StatusOK
		}
		return `{"error": "no fixture"}`, http.StatusNotFound
	})
	mbBase = server.URL
	manager := newManager(t, t.TempDir())

	single, err := manager.Resolve(context.Background(), "Test Artist", "Test Album", 2001, "", "", 1)
	if err != nil {
		t.Fatalf("resolve with limit 1: %v", err)
	}
	if len(single) != 1 {
		t.Fatalf("candidates = %d, want 1", len(single))
	}
	defaulted, err := manager.Resolve(context.Background(), "Test Artist", "Test Album", 2001, "", "", 0)
	if err != nil {
		t.Fatalf("resolve with default limit: %v", err)
	}
	if len(defaulted) != 5 {
		t.Fatalf("candidates = %d, want 5", len(defaulted))
	}
}

func manyGroupsBody(count int) string {
	groups := make([]mb.SearchReleaseGroup, 0, count)
	for i := 0; i < count; i++ {
		groups = append(groups, mb.SearchReleaseGroup{
			ID:               fmt.Sprintf("rg-%d", i),
			Score:            100,
			Title:            "Test Album",
			FirstReleaseDate: "2001",
			PrimaryType:      "Album",
			ArtistCredit:     []mb.ArtistCredit{{Name: "Test Artist"}},
		})
	}
	data, _ := json.Marshal(map[string]any{"release-groups": groups})
	return string(data)
}

func TestImportAutoByPerfectTrackList(t *testing.T) {
	// the candidate's name matches badly, but every file title matches a
	// distinct release track and the counts agree: identity
	startMB(t, mbFixture{
		search: []mb.SearchRelease{mbtest.SyntheticSearch("rel-x", "rg-x", "Completely Different Name", "Someone Else", "", 2)},
		releases: []mb.Release{
			mbtest.SyntheticRelease("rel-x", "rg-x", "Completely Different Name", "Someone Else", "",
				mb.ReleaseMedia{Position: 1, Format: "CD", TrackCount: 2, Tracks: []mb.ReleaseTrack{
					mbtest.Track(1, "First Song"), mbtest.Track(2, "Second Song"),
				}}),
		},
	})
	root := t.TempDir()
	src := twoTestFiles(t)
	manager := newManager(t, root)

	result, err := manager.Import(context.Background(), Request{Dir: src})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Status != statusImported {
		t.Fatalf("status: got %q (%+v), want imported by perfect track list", result.Status, result)
	}
}
