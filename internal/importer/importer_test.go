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
			mbtest.SyntheticSearch("rel-2", "rg-2", "Test Album", "Test Artist", "2019-03-03", 2),
		},
		releases: []mb.Release{
			mbtest.SyntheticRelease("rel-1", "rg-1", "Test Album", "Other Artist", "2001",
				mb.ReleaseMedia{Position: 1, TrackCount: 5}),
			// the right release carries a mismatched year, so auto is refused
			mbtest.SyntheticRelease("rel-2", "rg-2", "Test Album", "Test Artist", "2019-03-03",
				mb.ReleaseMedia{Position: 1, Format: "CD", TrackCount: 2, Tracks: []mb.ReleaseTrack{
					mbtest.Track(1, "First Song"), mbtest.Track(2, "Second Song"),
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
	if decision.Candidates[0].ReleaseID != "rel-1" {
		t.Errorf("top candidate: got %s, want rel-1 (oldest date preferred)", decision.Candidates[0].ReleaseID)
	}
	if len(manager.Decisions()) != 1 {
		t.Errorf("decisions list: got %d, want 1", len(manager.Decisions()))
	}

	if _, err := manager.Decide(ctx, decision.Token, 99, false); err == nil {
		t.Errorf("pick out of range: got nil error, want failure")
	}
	if _, err := manager.Decide(ctx, "nope", 1, false); err == nil {
		t.Errorf("unknown token: got nil error, want failure")
	}

	picked, err := manager.Decide(ctx, decision.Token, 2, false)
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
	skipped, err := manager.Decide(ctx, result.Decision.Token, 0, true)
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

	picked, err := manager.Decide(ctx, result.Decision.Token, 1, false)
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
	if len(result.Notes) == 0 || !strings.Contains(result.Notes[0], "does not match any unclaimed release track") {
		t.Errorf("notes should explain the rejection: %+v", result.Notes)
	}

	token := result.Decision.Token
	reparked, err := manager.Decide(context.Background(), token, 1, false)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if reparked.Status != statusNeedsDecision || reparked.Decision.Token != token {
		t.Fatalf("decide should re-park with the same token: %+v", reparked)
	}
	if len(reparked.Notes) == 0 || !strings.Contains(reparked.Notes[0], "does not match any unclaimed release track") {
		t.Errorf("re-park notes should explain: %+v", reparked.Notes)
	}
	skipped, err := manager.Decide(context.Background(), token, 0, true)
	if err != nil || skipped.Status != statusSkipped {
		t.Fatalf("skip after re-park: %+v err %v", skipped, err)
	}
}
