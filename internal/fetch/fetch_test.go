package fetch_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"doppel.moe/katydid/internal/api"
	"doppel.moe/katydid/internal/fetch"
	"doppel.moe/katydid/internal/importer"
	"doppel.moe/katydid/internal/library"
	"doppel.moe/katydid/internal/match"
	"doppel.moe/katydid/internal/slskd"
)

type fakeSlskd struct {
	searches    map[string]*slskd.Search
	enqueued    map[string][]slskd.File
	downloads   []slskd.UserResponse
	createErr   error
	enqueuedSeq []string
	failEnqueue map[string]error
}

func newFakeSlskd() *fakeSlskd {
	return &fakeSlskd{searches: map[string]*slskd.Search{}, enqueued: map[string][]slskd.File{}}
}

func (f *fakeSlskd) CreateSearch(_ context.Context, req slskd.SearchRequest) (*slskd.Search, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	search := &slskd.Search{ID: fmt.Sprintf("search-%d", len(f.searches)+1), SearchText: req.SearchText}
	f.searches[search.ID] = search
	return search, nil
}

func (f *fakeSlskd) Search(_ context.Context, id string, _ bool) (*slskd.Search, error) {
	search, ok := f.searches[id]
	if !ok {
		return nil, errors.New("no such search")
	}
	return search, nil
}

func (f *fakeSlskd) DeleteSearch(_ context.Context, id string) error {
	delete(f.searches, id)
	return nil
}

func (f *fakeSlskd) EnqueueDownloads(_ context.Context, username string, files []slskd.File) error {
	if err := f.failEnqueue[username]; err != nil {
		return err
	}
	f.enqueued[username] = append(f.enqueued[username], files...)
	f.enqueuedSeq = append(f.enqueuedSeq, username)
	return nil
}

func (f *fakeSlskd) Downloads(_ context.Context) ([]slskd.UserResponse, error) {
	return f.downloads, nil
}

type fakeKatyd struct {
	resolve         api.ResolveResponse
	resolveErr      error
	resolves        int
	imported        []importer.Request
	importDirFiles  [][]string
	importResult    importer.Result
	decided         []string
	decideResult    importer.Result
	recorded        map[string]importer.Result
	resolveGroup    api.ResolveResponse
	resolveGroupFor string
	albums          api.AlbumsResponse
}

func (f *fakeKatyd) Resolve(_ string, _ string, _ int, _ string) (api.ResolveResponse, error) {
	f.resolves++
	return f.resolve, f.resolveErr
}

func (f *fakeKatyd) Albums(_ library.Query) (api.AlbumsResponse, error) {
	return f.albums, nil
}

func (f *fakeKatyd) Import(req importer.Request) (importer.Result, error) {
	f.imported = append(f.imported, req)
	if entries, err := os.ReadDir(req.Dir); err == nil {
		names := []string{}
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		f.importDirFiles = append(f.importDirFiles, names)
	}
	return f.importResult, nil
}

func (f *fakeKatyd) Decide(token string, _ importer.DecideInput) (importer.Result, error) {
	f.decided = append(f.decided, token)
	return f.decideResult, nil
}

func harness(t *testing.T, slskdFake *fakeSlskd, katydFake *fakeKatyd, tune ...func(*fetch.Config)) (*fetch.Orchestrator, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := fetch.OpenStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	cfg := fetch.Config{
		Slskd:        slskdFake,
		Katyd:        katydFake,
		DownloadsDir: filepath.Join(dir, "downloads"),
	}
	for _, f := range tune {
		f(&cfg)
	}
	return fetch.New(store, cfg), dir
}

func find(t *testing.T, orchestrator *fetch.Orchestrator, id string) fetch.Want {
	t.Helper()
	want, ok := orchestrator.Want(id)
	if !ok {
		t.Fatalf("want %s vanished", id)
	}
	return want
}

func TestQueuedToSearchingOnAutoResolve(t *testing.T) {
	slskdFake := newFakeSlskd()
	katydFake := autoTitlesResolver("vault", "orbit")
	orchestrator, _ := harness(t, slskdFake, katydFake)
	want, err := orchestrator.Add(fetch.Spec{Artist: "saetia", Album: "saetia", Year: 1998, GroupID: ""})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateSearching {
		t.Fatalf("state = %q, want searching", want.State)
	}
	if want.TrackCount != 2 {
		t.Fatalf("track count = %d, want 2", want.TrackCount)
	}
	if len(slskdFake.searches) != 1 {
		t.Fatalf("searches = %d, want 1", len(slskdFake.searches))
	}
}

func TestWantAlreadyInLibrarySkipsFetch(t *testing.T) {
	slskdFake := newFakeSlskd()
	katydFake := autoTitlesResolver("vault", "orbit")
	katydFake.albums = api.AlbumsResponse{Albums: []library.Album{{
		ID:   "Saetia/1998 - Saetia",
		Meta: library.Meta{ReleaseID: "r1"},
	}}}
	orchestrator, _ := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add(fetch.Spec{Artist: "saeita", Album: "saeita", Year: 0, GroupID: "rg-1"})
	orchestrator.Tick(context.Background())
	got := find(t, orchestrator, want.ID)
	if got.State != fetch.StateImported || got.AlbumID != "Saetia/1998 - Saetia" {
		t.Fatalf("state %q album %q err %q, want imported without fetching", got.State, got.AlbumID, got.Error)
	}
	if len(slskdFake.enqueuedSeq) != 0 {
		t.Fatalf("nothing should be downloaded: %+v", slskdFake.enqueuedSeq)
	}
	if len(got.Notes) == 0 || !strings.Contains(got.Notes[0], "already in the library") {
		t.Fatalf("notes should explain the skip: %+v", got.Notes)
	}
}

func TestResolvedNamesRecordedTypedKept(t *testing.T) {
	slskdFake := newFakeSlskd()
	katydFake := autoTitlesResolver("vault", "orbit")
	katydFake.resolveGroup.Candidates[0].Artist = "Saetia"
	katydFake.resolveGroup.Candidates[0].Title = "Saetia"
	orchestrator, _ := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add(fetch.Spec{Artist: "saeita", Album: "saeita", Year: 0, GroupID: "rg-1"})
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.ReleaseArtist != "Saetia" || want.ReleaseTitle != "Saetia" {
		t.Fatalf("resolved names = %q / %q", want.ReleaseArtist, want.ReleaseTitle)
	}
	if want.Artist != "saeita" || want.Album != "saeita" {
		t.Fatalf("typed specifier should be kept: %q / %q", want.Artist, want.Album)
	}
}

func TestLastFMWantSkipsResolveAndImportsMarked(t *testing.T) {
	slskdFake := newFakeSlskd()
	katydFake := autoTitlesResolver("vault", "orbit")
	orchestrator, root := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add(fetch.Spec{
		Artist: "the brown", Album: "mellow", Source: "lastfm",
		ReleaseArtist: "The Brown", ReleaseTitle: "MelloW",
		TrackTitles: []string{"Modern play", "森二潜ム"},
	})
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateSearching || katydFake.resolves != 0 {
		t.Fatalf("lastfm want should search without resolve: state %q resolves %d", want.State, katydFake.resolves)
	}
	search := slskdFake.searches[want.SearchID]
	if search.SearchText != "The Brown MelloW" {
		t.Fatalf("search text %q, want the last.fm names", search.SearchText)
	}
	search.IsComplete = true
	search.Responses = []slskd.Response{{Username: "peer", HasFreeUploadSlot: true, Files: []slskd.File{
		{Filename: `x\The Brown\MelloW\01 - Modern play.flac`, Size: 9},
		{Filename: `x\The Brown\MelloW\02 - 森二潜ム.flac`, Size: 19},
	}}}
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateDownloading || len(want.Enqueued) != 2 {
		t.Fatalf("state %q enqueued %d", want.State, len(want.Enqueued))
	}
	localDir := filepath.Join(root, "downloads", "MelloW")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "01 - Modern play.flac"), make([]byte, 9), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "02 - 森二潜ム.flac"), make([]byte, 19), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	slskdFake.downloads = []slskd.UserResponse{{Username: "peer", Directories: []slskd.DirectoryResponse{{Files: []slskd.Transfer{
		{ID: "t1", Filename: `x\The Brown\MelloW\01 - Modern play.flac`, Size: 9, State: "Completed, Succeeded"},
		{ID: "t2", Filename: `x\The Brown\MelloW\02 - 森二潜ム.flac`, Size: 19, State: "Completed, Succeeded"},
	}}}}}
	katydFake.importResult = importer.Result{Status: "imported", AlbumID: "The Brown/[last.fm]"}
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateImported {
		t.Fatalf("state %q err %q", want.State, want.Error)
	}
	if len(katydFake.imported) != 1 {
		t.Fatalf("imports: %+v", katydFake.imported)
	}
	req := katydFake.imported[0]
	if req.Source != "lastfm" || req.Artist != "The Brown" || req.Album != "MelloW" {
		t.Fatalf("import request should carry lastfm identity: %+v", req)
	}
}

func TestMissingFileRetriesThenFailsNamed(t *testing.T) {
	slskdFake := newFakeSlskd()
	katydFake := autoTitlesResolver("vault", "orbit")
	orchestrator, root := harness(t, slskdFake, katydFake, func(c *fetch.Config) { c.BindRetry = time.Nanosecond })
	want, _ := orchestrator.Add(fetch.Spec{Artist: "saetia", Album: "saetia", Year: 0, GroupID: ""})
	orchestrator.Tick(context.Background())
	w := find(t, orchestrator, want.ID)
	search := slskdFake.searches[w.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{{Username: "peer", Files: []slskd.File{
		{Filename: `dir\SAETIA\01 - vault.flac`, Size: 500},
		{Filename: `dir\SAETIA\02 - orbit.flac`, Size: 900},
	}}}
	orchestrator.Tick(context.Background())
	localDir := filepath.Join(root, "downloads", "SAETIA")
	os.MkdirAll(localDir, 0o755)
	os.WriteFile(filepath.Join(localDir, "01 - vault.flac"), make([]byte, 500), 0o644)
	slskdFake.downloads = []slskd.UserResponse{{Username: "peer", Directories: []slskd.DirectoryResponse{{Files: []slskd.Transfer{
		{ID: "t1", Filename: `dir\SAETIA\01 - vault.flac`, Size: 500, State: "Completed, Succeeded"},
		{ID: "t2", Filename: `dir\SAETIA\02 - orbit.flac`, Size: 900, State: "Completed, Succeeded"},
	}}}}}
	orchestrator.Tick(context.Background())
	w = find(t, orchestrator, want.ID)
	if w.State != fetch.StateDownloading || w.Error != "" {
		t.Fatalf("missing file should wait, not fail: state %q err %q", w.State, w.Error)
	}
	if w.Paths[`dir\SAETIA\01 - vault.flac`] == "" {
		t.Fatalf("found file should be bound: %+v", w.Paths)
	}
	orchestrator.Tick(context.Background())
	w = find(t, orchestrator, want.ID)
	if w.State != fetch.StateFailed || !strings.Contains(w.Error, "02 - orbit.flac") {
		t.Fatalf("stale missing file should fail naming it: state %q err %q", w.State, w.Error)
	}
}

func TestDuplicateCopyFailsNamingBoth(t *testing.T) {
	slskdFake := newFakeSlskd()
	katydFake := autoTitlesResolver("vault")
	orchestrator, root := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add(fetch.Spec{Artist: "saetia", Album: "saetia", Year: 0, GroupID: ""})
	orchestrator.Tick(context.Background())
	w := find(t, orchestrator, want.ID)
	search := slskdFake.searches[w.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{{Username: "peer", Files: []slskd.File{
		{Filename: `dir\SAETIA\01 - vault.flac`, Size: 500},
	}}}
	orchestrator.Tick(context.Background())
	for _, dir := range []string{"SAETIA", "SAETIA.1"} {
		os.MkdirAll(filepath.Join(root, "downloads", dir), 0o755)
		os.WriteFile(filepath.Join(root, "downloads", dir, "01 - vault.flac"), make([]byte, 500), 0o644)
	}
	slskdFake.downloads = []slskd.UserResponse{{Username: "peer", Directories: []slskd.DirectoryResponse{{Files: []slskd.Transfer{
		{ID: "t1", Filename: `dir\SAETIA\01 - vault.flac`, Size: 500, State: "Completed, Succeeded"},
	}}}}}
	orchestrator.Tick(context.Background())
	w = find(t, orchestrator, want.ID)
	if w.State != fetch.StateFailed || !strings.Contains(w.Error, "found at") || !strings.Contains(w.Error, "delete the stale copy") {
		t.Fatalf("duplicate copies should fail loud: state %q err %q", w.State, w.Error)
	}
}

func TestVanishedBindingRebindsAfterReboot(t *testing.T) {
	slskdFake := newFakeSlskd()
	katydFake := autoTitlesResolver("vault", "orbit")
	orchestrator, root := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add(fetch.Spec{Artist: "saetia", Album: "saetia", Year: 0, GroupID: ""})
	orchestrator.Tick(context.Background())
	w := find(t, orchestrator, want.ID)
	search := slskdFake.searches[w.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{{Username: "peer", Files: []slskd.File{
		{Filename: `dir\SAETIA\01 - vault.flac`, Size: 500},
		{Filename: `dir\SAETIA\02 - orbit.flac`, Size: 900},
	}}}
	orchestrator.Tick(context.Background())
	localDir := filepath.Join(root, "downloads", "SAETIA")
	os.MkdirAll(localDir, 0o755)
	file := filepath.Join(localDir, "01 - vault.flac")
	os.WriteFile(file, make([]byte, 500), 0o644)
	// track 2 is still transferring: track 1 can bind but nothing imports
	slskdFake.downloads = []slskd.UserResponse{{Username: "peer", Directories: []slskd.DirectoryResponse{{Files: []slskd.Transfer{
		{ID: "t1", Filename: `dir\SAETIA\01 - vault.flac`, Size: 500, State: "Completed, Succeeded"},
		{ID: "t2", Filename: `dir\SAETIA\02 - orbit.flac`, Size: 900, State: "Downloading"},
	}}}}}
	orchestrator.Tick(context.Background())
	w = find(t, orchestrator, want.ID)
	if w.Paths[`dir\SAETIA\01 - vault.flac`] == "" {
		t.Fatalf("file should be bound: %+v", w.Paths)
	}
	// tmpfs wiped the tree (reboot): the binding dangles and must drop,
	// and nothing may import from dangling links
	os.Remove(file)
	orchestrator.Tick(context.Background())
	w = find(t, orchestrator, want.ID)
	if w.Paths != nil && w.Paths[`dir\SAETIA\01 - vault.flac`] != "" {
		t.Fatalf("dangling binding should be dropped: %+v", w.Paths)
	}
	if w.StagingDir != "" {
		t.Fatalf("staging must not be built from dangling bindings: %q", w.StagingDir)
	}
	if w.State != fetch.StateDownloading {
		t.Fatalf("want should wait for the rebind: state %q err %q", w.State, w.Error)
	}
	// the file lands again and the slot rebinds
	os.WriteFile(file, make([]byte, 500), 0o644)
	orchestrator.Tick(context.Background())
	w = find(t, orchestrator, want.ID)
	if w.Paths[`dir\SAETIA\01 - vault.flac`] == "" {
		t.Fatalf("file should rebind: %+v", w.Paths)
	}
}

func TestTerminalWantsReapWithTheirFiles(t *testing.T) {
	slskdFake := newFakeSlskd()
	katydFake := autoTitlesResolver("vault")
	orchestrator, root := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add(fetch.Spec{Artist: "saetia", Album: "saetia", Year: 0, GroupID: ""})
	orchestrator.Tick(context.Background())
	w := find(t, orchestrator, want.ID)
	search := slskdFake.searches[w.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{{Username: "peer", Files: []slskd.File{
		{Filename: `dir\SAETIA\01 - vault.flac`, Size: 500},
	}}}
	orchestrator.Tick(context.Background())
	localDir := filepath.Join(root, "downloads", "SAETIA")
	os.MkdirAll(localDir, 0o755)
	file := filepath.Join(localDir, "01 - vault.flac")
	os.WriteFile(file, make([]byte, 500), 0o644)
	slskdFake.downloads = []slskd.UserResponse{{Username: "peer", Directories: []slskd.DirectoryResponse{{Files: []slskd.Transfer{
		{ID: "t1", Filename: `dir\SAETIA\01 - vault.flac`, Size: 500, State: "Completed, Succeeded"},
	}}}}}
	katydFake.importResult = importer.Result{Status: "imported", AlbumID: "x"}
	orchestrator.Tick(context.Background())
	w = find(t, orchestrator, want.ID)
	if w.State != fetch.StateImported {
		t.Fatalf("state %q err %q", w.State, w.Error)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("imported want should purge its download: %v", err)
	}
	// reopen with an expired window, as after a config change plus restart
	store, err := fetch.OpenStore(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	expired := fetch.New(store, fetch.Config{
		Slskd:          slskdFake,
		Katyd:          katydFake,
		DownloadsDir:   filepath.Join(root, "downloads"),
		ExpireTerminal: time.Nanosecond,
	})
	expired.Tick(context.Background())
	if _, ok := expired.Want(want.ID); ok {
		t.Fatalf("expired imported want should be reaped")
	}
}

func TestNeedsDecisionExpiresAfterADay(t *testing.T) {
	slskdFake := newFakeSlskd()
	katydFake := autoTitlesResolver("vault")
	orchestrator, root := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add(fetch.Spec{Artist: "saetia", Album: "saetia", Year: 0, GroupID: ""})
	orchestrator.Tick(context.Background())
	w := find(t, orchestrator, want.ID)
	search := slskdFake.searches[w.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{{Username: "peer", Files: []slskd.File{
		{Filename: `dir\SAETIA\01 - vault.flac`, Size: 500},
	}}}
	orchestrator.Tick(context.Background())
	localDir := filepath.Join(root, "downloads", "SAETIA")
	os.MkdirAll(localDir, 0o755)
	file := filepath.Join(localDir, "01 - vault.flac")
	os.WriteFile(file, make([]byte, 500), 0o644)
	slskdFake.downloads = []slskd.UserResponse{{Username: "peer", Directories: []slskd.DirectoryResponse{{Files: []slskd.Transfer{
		{ID: "t1", Filename: `dir\SAETIA\01 - vault.flac`, Size: 500, State: "Completed, Succeeded"},
	}}}}}
	katydFake.importResult = importer.Result{Status: "needs_decision", Decision: &importer.Decision{Token: "tok-1"}}
	orchestrator.Tick(context.Background())
	w = find(t, orchestrator, want.ID)
	if w.State != fetch.StateNeedsPick {
		t.Fatalf("state %q, want needs_decision", w.State)
	}
	// still inside the window: files must survive the parked decision
	orchestrator.Tick(context.Background())
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("parked decision needs its files: %v", err)
	}
	if _, ok := orchestrator.Want(want.ID); !ok {
		t.Fatalf("parked want should survive inside the window")
	}
	// reopen the store with an expired window, as after a config change
	// plus restart; the decision is gone and the downloads with it
	store, err := fetch.OpenStore(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	expired := fetch.New(store, fetch.Config{
		Slskd:               slskdFake,
		Katyd:               katydFake,
		DownloadsDir:        filepath.Join(root, "downloads"),
		ExpireNeedsDecision: time.Nanosecond,
		ExpireTerminal:      time.Hour,
	})
	expired.Tick(context.Background())
	w = find(t, expired, want.ID)
	if w.State != fetch.StateFailed || !strings.Contains(w.Error, "expired") {
		t.Fatalf("expired decision should fail the want: state %q err %q", w.State, w.Error)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("expired decision should purge its downloads: %v", err)
	}
}

func TestZombieTransfersBindFromDisk(t *testing.T) {
	// the tmpfs-fill incident: slskd's transfer entries never went
	// terminal, but every file is on disk — the want must finish anyway
	slskdFake := newFakeSlskd()
	katydFake := autoTitlesResolver("vault")
	katydFake.importResult = importer.Result{Status: "imported", AlbumID: "x"}
	orchestrator, root := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add(fetch.Spec{Artist: "saetia", Album: "saetia", Year: 0, GroupID: ""})
	orchestrator.Tick(context.Background())
	w := find(t, orchestrator, want.ID)
	search := slskdFake.searches[w.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{{Username: "peer", Files: []slskd.File{
		{Filename: `dir\SAETIA\01 - vault.flac`, Size: 500},
	}}}
	orchestrator.Tick(context.Background())
	localDir := filepath.Join(root, "downloads", "SAETIA")
	os.MkdirAll(localDir, 0o755)
	os.WriteFile(filepath.Join(localDir, "01 - vault.flac"), make([]byte, 500), 0o644)
	slskdFake.downloads = []slskd.UserResponse{{Username: "peer", Directories: []slskd.DirectoryResponse{{Files: []slskd.Transfer{
		{ID: "t1", Filename: `dir\SAETIA\01 - vault.flac`, Size: 500, State: "Queued"},
	}}}}}
	orchestrator.Tick(context.Background())
	w = find(t, orchestrator, want.ID)
	if w.State != fetch.StateImported {
		t.Fatalf("files on disk should import despite zombie transfers: state %q err %q", w.State, w.Error)
	}
}

func TestPeerSwitchKeepsStagedFiles(t *testing.T) {
	// one slot bound, one permanently failing: exhausting retries must
	// one slot bound, one permanently failing: exhausting retries hands
	// just that slot to failover (searching), never touching the staged
	// file and never switching the whole want
	slskdFake := newFakeSlskd()
	katydFake := autoTitlesResolver("vault", "orbit")
	orchestrator, root := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add(fetch.Spec{Artist: "saetia", Album: "saetia", Year: 0, GroupID: ""})
	orchestrator.Tick(context.Background())
	w := find(t, orchestrator, want.ID)
	search := slskdFake.searches[w.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{
		{Username: "peer", Files: []slskd.File{
			{Filename: `dir\SAETIA\01 - vault.flac`, Size: 500},
			{Filename: `dir\SAETIA\02 - orbit.flac`, Size: 900},
		}},
		{Username: "other", Files: []slskd.File{
			{Filename: `x\saetia\02 - orbit (other rip).flac`, Size: 910},
		}},
	}
	orchestrator.Tick(context.Background())
	localDir := filepath.Join(root, "downloads", "SAETIA")
	os.MkdirAll(localDir, 0o755)
	staged := filepath.Join(localDir, "01 - vault.flac")
	os.WriteFile(staged, make([]byte, 500), 0o644)
	slskdFake.downloads = []slskd.UserResponse{{Username: "peer", Directories: []slskd.DirectoryResponse{{Files: []slskd.Transfer{
		{ID: "t1", Filename: `dir\SAETIA\01 - vault.flac`, Size: 500, State: "Completed, Succeeded"},
		{ID: "t2", Filename: `dir\SAETIA\02 - orbit.flac`, Size: 900, State: "Completed, Errored", Exception: strPtr("File not shared.")},
	}}}}}
	for i := 0; i < 3; i++ {
		orchestrator.Tick(context.Background())
		w = find(t, orchestrator, want.ID)
		if w.State != fetch.StateDownloading {
			t.Fatalf("tick %d: state %q err %q, want still retrying", i+1, w.State, w.Error)
		}
		if len(w.ExcludedPeers) != 0 {
			t.Fatalf("tick %d: a single file failure must not switch the peer", i+1)
		}
	}
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("staged file must survive: %v", err)
	}
	orchestrator.Tick(context.Background())
	w = find(t, orchestrator, want.ID)
	if w.State != fetch.StateSearching || len(w.Excluded) != 1 || w.Excluded[`dir\SAETIA\02 - orbit.flac`] != "peer" {
		t.Fatalf("fourth failure should hand the slot to failover: state %q excluded %v", w.State, w.Excluded)
	}
	orchestrator.Tick(context.Background())
	w = find(t, orchestrator, want.ID)
	if w.State != fetch.StateDownloading || w.Owners[`x\saetia\02 - orbit (other rip).flac`] != "other" {
		t.Fatalf("orbit should be re-sourced from other: state %q owners %v err %q", w.State, w.Owners, w.Error)
	}
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("staged file must survive failover: %v", err)
	}
	if len(w.Enqueued) != 2 {
		t.Fatalf("enqueued should be 2 (1 kept + 1 re-sourced): %d", len(w.Enqueued))
	}
}

func TestFlacPoolSplitsAcrossPeers(t *testing.T) {
	slskdFake := newFakeSlskd()
	katydFake := autoTitlesResolver("vault", "orbit")
	orchestrator, root := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add(fetch.Spec{Artist: "saetia", Album: "saetia", Year: 0, GroupID: ""})
	orchestrator.Tick(context.Background())
	w := find(t, orchestrator, want.ID)
	search := slskdFake.searches[w.SearchID]
	search.IsComplete = true
	// a 4-track album, no single flac host complete; two partilists
	// whose union covers it (meets the 2-tracks-per-lane minimum),
	// lossy full-album host present but outclassed per policy
	katydFake = autoTitlesResolver("vault", "orbit", "storm", "ground")
	orchestrator, root = harness(t, slskdFake, katydFake)
	want, _ = orchestrator.Add(fetch.Spec{Artist: "saetia", Album: "saetia", Year: 0, GroupID: ""})
	orchestrator.Tick(context.Background())
	w = find(t, orchestrator, want.ID)
	search = slskdFake.searches[w.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{
		{Username: "halfa", HasFreeUploadSlot: true, Files: []slskd.File{
			{Filename: `a\SAETIA\01 - vault.flac`, Size: 9},
			{Filename: `a\SAETIA\02 - orbit.flac`, Size: 19},
		}},
		{Username: "halfb", Files: []slskd.File{
			{Filename: `b\saetia\03 - storm.flac`, Size: 29},
			{Filename: `b\saetia\04 - ground.flac`, Size: 39},
		}},
		{Username: "lossy", Files: []slskd.File{
			{Filename: `c\saetia\01 - vault.mp3`, Size: 5},
			{Filename: `c\saetia\03 - storm.mp3`, Size: 7},
		}},
	}
	orchestrator.Tick(context.Background())
	w = find(t, orchestrator, want.ID)
	if w.State != fetch.StateDownloading {
		t.Fatalf("state %q err %q", w.State, w.Error)
	}
	if w.Owners[`a\SAETIA\01 - vault.flac`] != "halfa" || w.Owners[`b\saetia\03 - storm.flac`] != "halfb" {
		t.Fatalf("flac slots should split across the two flac peers: %v", w.Owners)
	}
	if len(slskdFake.enqueued["halfa"]) != 2 || len(slskdFake.enqueued["halfb"]) != 2 {
		t.Fatalf("each pool peer should carry two files: %v", slskdFake.enqueued)
	}
	if len(slskdFake.enqueued["lossy"]) != 0 {
		t.Fatalf("lossy host must never join a flac pool: %v", slskdFake.enqueued["lossy"])
	}
	held := false
	for _, note := range w.Notes {
		if strings.Contains(note, "pooling") {
			held = true
		}
	}
	if !held {
		t.Fatalf("pooling should be noted: %+v", w.Notes)
	}
	// all four files land and import as one album
	os.MkdirAll(filepath.Join(root, "downloads", "a"), 0o755)
	os.MkdirAll(filepath.Join(root, "downloads", "b"), 0o755)
	os.WriteFile(filepath.Join(root, "downloads", "a", "01 - vault.flac"), make([]byte, 9), 0o644)
	os.WriteFile(filepath.Join(root, "downloads", "a", "02 - orbit.flac"), make([]byte, 19), 0o644)
	os.WriteFile(filepath.Join(root, "downloads", "b", "03 - storm.flac"), make([]byte, 29), 0o644)
	os.WriteFile(filepath.Join(root, "downloads", "b", "04 - ground.flac"), make([]byte, 39), 0o644)
	slskdFake.downloads = []slskd.UserResponse{
		{Username: "halfa", Directories: []slskd.DirectoryResponse{{Files: []slskd.Transfer{
			{ID: "t1", Filename: `a\SAETIA\01 - vault.flac`, Size: 9, State: "Completed, Succeeded"},
			{ID: "t2", Filename: `a\SAETIA\02 - orbit.flac`, Size: 19, State: "Completed, Succeeded"},
		}}}},
		{Username: "halfb", Directories: []slskd.DirectoryResponse{{Files: []slskd.Transfer{
			{ID: "t3", Filename: `b\saetia\03 - storm.flac`, Size: 29, State: "Completed, Succeeded"},
			{ID: "t4", Filename: `b\saetia\04 - ground.flac`, Size: 39, State: "Completed, Succeeded"},
		}}}},
	}
	katydFake.importResult = importer.Result{Status: "imported", AlbumID: "x"}
	orchestrator.Tick(context.Background())
	w = find(t, orchestrator, want.ID)
	if w.State != fetch.StateImported {
		t.Fatalf("pooled album should import: state %q err %q", w.State, w.Error)
	}
	if got := katydFake.importDirFiles[0]; len(got) != 4 {
		t.Fatalf("staged %v, want all four pool files", got)
	}
}

func TestCompleteHostJoinsPool(t *testing.T) {
	// one flac host with the whole album plus two partilists: the pool
	// splits across all three — speed comes from lanes, flac is flac.
	// The complete host is capped at its slice so it cannot swallow
	// every track and defeat the parallelism.
	slskdFake := newFakeSlskd()
	katydFake := autoTitlesResolver("vault", "orbit", "storm", "ground")
	orchestrator, _ := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add(fetch.Spec{Artist: "saetia", Album: "saetia", Year: 0, GroupID: ""})
	orchestrator.Tick(context.Background())
	w := find(t, orchestrator, want.ID)
	search := slskdFake.searches[w.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{
		{Username: "full", HasFreeUploadSlot: true, Files: []slskd.File{
			{Filename: `f\01 - vault.flac`, Size: 9},
			{Filename: `f\02 - orbit.flac`, Size: 19},
			{Filename: `f\03 - storm.flac`, Size: 29},
			{Filename: `f\04 - ground.flac`, Size: 39},
		}},
		{Username: "halfa", Files: []slskd.File{
			{Filename: `a\01 - vault.flac`, Size: 9},
			{Filename: `a\02 - orbit.flac`, Size: 19},
		}},
		{Username: "halfb", Files: []slskd.File{
			{Filename: `b\03 - storm.flac`, Size: 29},
			{Filename: `b\04 - ground.flac`, Size: 39},
		}},
	}
	orchestrator.Tick(context.Background())
	w = find(t, orchestrator, want.ID)
	if w.State != fetch.StateDownloading {
		t.Fatalf("state %q err %q", w.State, w.Error)
	}
	// lanes = min(4 tracks/2 min share, maxLanes, peers) = 2, taken from
	// the biggest flac contributors: full and halfa. Each track goes to
	// the least-loaded feasible lane; halfa only holds two of the four
	// tracks, so it ends up with one and full with three.
	if w.Owners[`a\02 - orbit.flac`] != "halfa" {
		t.Fatalf("halfa should carry orbit: %v", w.Owners)
	}
	if w.Owners[`f\01 - vault.flac`] != "full" || w.Owners[`f\03 - storm.flac`] != "full" {
		t.Fatalf("full should carry its lanes: %v", w.Owners)
	}
	if len(slskdFake.enqueued["full"]) != 3 || len(slskdFake.enqueued["halfa"]) != 1 {
		t.Fatalf("lane shares off: %v", slskdFake.enqueued)
	}
	if len(slskdFake.enqueued["halfb"]) != 0 {
		t.Fatalf("only the top-2 flac peers should be lanes: %v", slskdFake.enqueued)
	}
}

func TestFlacGapFallsBackToSingleHost(t *testing.T) {
	slskdFake := newFakeSlskd()
	katydFake := autoTitlesResolver("vault", "orbit")
	orchestrator, _ := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add(fetch.Spec{Artist: "saetia", Album: "saetia", Year: 0, GroupID: ""})
	orchestrator.Tick(context.Background())
	w := find(t, orchestrator, want.ID)
	search := slskdFake.searches[w.SearchID]
	search.IsComplete = true
	// the flac union misses a track, but a lossy host has everything:
	// per policy the lossy host takes the whole album, unsplit
	search.Responses = []slskd.Response{
		{Username: "halfflac", Files: []slskd.File{
			{Filename: `a\01 - vault.flac`, Size: 9},
		}},
		{Username: "allmp3", Files: []slskd.File{
			{Filename: `b\01 - vault.mp3`, Size: 5},
			{Filename: `b\02 - orbit.mp3`, Size: 6},
		}},
	}
	orchestrator.Tick(context.Background())
	w = find(t, orchestrator, want.ID)
	if w.State != fetch.StateDownloading {
		t.Fatalf("state %q err %q", w.State, w.Error)
	}
	if len(slskdFake.enqueued["allmp3"]) != 2 {
		t.Fatalf("lossy host should carry the whole album: %v", slskdFake.enqueued)
	}
	if len(slskdFake.enqueued["halfflac"]) != 0 {
		t.Fatalf("a flac gap disqualifies the pool entirely: %v", slskdFake.enqueued)
	}
	for _, file := range w.Enqueued {
		if w.Owners[file.Filename] != "allmp3" {
			t.Fatalf("everything should belong to the lossy host: %v", w.Owners)
		}
	}
}

func TestPoolSlotFailsOverToFlacPeer(t *testing.T) {
	slskdFake := newFakeSlskd()
	katydFake := autoTitlesResolver("vault", "orbit", "storm", "ground")
	orchestrator, _ := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add(fetch.Spec{Artist: "saetia", Album: "saetia", Year: 0, GroupID: ""})
	orchestrator.Tick(context.Background())
	w := find(t, orchestrator, want.ID)
	search := slskdFake.searches[w.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{
		{Username: "halfa", Files: []slskd.File{
			{Filename: `a\01 - vault.flac`, Size: 9},
			{Filename: `a\02 - orbit.flac`, Size: 19},
		}},
		{Username: "halfb", Files: []slskd.File{
			{Filename: `b\03 - storm.flac`, Size: 29},
			{Filename: `b\04 - ground.flac`, Size: 39},
		}},
		{Username: "halfc", Files: []slskd.File{
			{Filename: `c\01 - vault.flac`, Size: 12},
			{Filename: `c\02 - orbit.flac`, Size: 22},
		}},
	}
	orchestrator.Tick(context.Background())
	w = find(t, orchestrator, want.ID)
	if w.Owners[`a\01 - vault.flac`] != "halfa" || w.Owners[`b\03 - storm.flac`] != "halfb" {
		t.Fatalf("initial split wrong: %v", w.Owners)
	}
	// halfa permanently refuses its files; both slots move to halfc's
	// flac (the load-preference would pick halfb for one, but halfb
	// lacks those tracks), halfb's own slots untouched
	slskdFake.downloads = []slskd.UserResponse{{Username: "halfa", Directories: []slskd.DirectoryResponse{{Files: []slskd.Transfer{
		{ID: "t1", Filename: `a\01 - vault.flac`, Size: 9, State: "Completed, Errored", Exception: strPtr("File not shared.")},
		{ID: "t2", Filename: `a\02 - orbit.flac`, Size: 19, State: "Completed, Errored", Exception: strPtr("File not shared.")},
	}}}}}
	for i := 0; i < 4; i++ {
		orchestrator.Tick(context.Background())
	}
	// tick 5: the failover round re-sources the dead slots
	orchestrator.Tick(context.Background())
	w = find(t, orchestrator, want.ID)
	if w.Owners[`c\01 - vault.flac`] != "halfc" || w.Owners[`c\02 - orbit.flac`] != "halfc" {
		t.Fatalf("failed slots should move to halfc's flac: owners %v state %q err %q", w.Owners, w.State, w.Error)
	}
	if w.Owners[`b\03 - storm.flac`] != "halfb" {
		t.Fatalf("healthy slots should be untouched: %v", w.Owners)
	}
	if len(w.Enqueued) != 4 {
		t.Fatalf("enqueued %d, want all four tracks still planned", len(w.Enqueued))
	}
}

func TestPinnedGroupSkipsGroupSearch(t *testing.T) {
	slskdFake := newFakeSlskd()
	katydFake := autoTitlesResolver("vault", "orbit")
	orchestrator, _ := harness(t, slskdFake, katydFake)
	want, err := orchestrator.Add(fetch.Spec{Artist: "saetia", Album: "saetia", Year: 0, GroupID: "rg-1"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateSearching {
		t.Fatalf("state = %q (%s), want searching", want.State, want.Error)
	}
	if want.GroupID != "rg-1" {
		t.Fatalf("group = %q, want the pinned rg-1", want.GroupID)
	}
	if want.TrackCount != 2 || len(want.TrackTitles) != 2 {
		t.Fatalf("track shape = %d titles %d, want the pinned group release", want.TrackCount, len(want.TrackTitles))
	}
	if katydFake.resolves != 0 {
		t.Fatalf("group search resolve calls = %d, want 0", katydFake.resolves)
	}
}

func TestLowConfidenceGatesOnReleasePick(t *testing.T) {
	slskdFake := newFakeSlskd()
	katydFake := &fakeKatyd{
		resolve: api.ResolveResponse{
			Candidates: []match.Candidate{
				{ReleaseID: "r1", TitleSim: 0.7, ArtistSim: 1},
				{ReleaseID: "r2", TitleSim: 0.65, ArtistSim: 0.9},
			},
			Auto: false,
		},
		resolveGroup:    api.ResolveResponse{Candidates: []match.Candidate{{ReleaseID: "r2-rel", TrackCount: 12, TitleSim: 1, ArtistSim: 1, TrackTitles: []string{"A", "B"}}}, Auto: true},
		resolveGroupFor: "r2",
	}
	orchestrator, _ := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add(fetch.Spec{Artist: "drukqs", Album: "drukqs", Year: 2001, GroupID: ""})
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateNeedsRelease {
		t.Fatalf("state = %q, want needs_release", want.State)
	}
	if len(slskdFake.searches) != 0 {
		t.Fatalf("no search should exist before the pick")
	}
	decided, err := orchestrator.Decide(context.Background(), want.ID, 2, false)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if decided.State != fetch.StateSearching {
		t.Fatalf("state = %q, want searching", decided.State)
	}
	if decided.TrackCount != 12 {
		t.Fatalf("track count = %d, want 12 from picked candidate", decided.TrackCount)
	}
	if _, err := orchestrator.Decide(context.Background(), want.ID, 1, false); err == nil {
		t.Fatalf("second decide on searching want should fail")
	}
}

func TestSearchToDownloadPicksBestDirectory(t *testing.T) {
	slskdFake := newFakeSlskd()
	katydFake := autoTitlesResolver("vault", "orbit")
	orchestrator, _ := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add(fetch.Spec{Artist: "saetia", Album: "saetia", Year: 0, GroupID: ""})
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	searchID := want.SearchID
	slskdFake.searches[searchID].IsComplete = true
	slskdFake.searches[searchID].Responses = []slskd.Response{
		{
			Username: "flacless", HasFreeUploadSlot: true, UploadSpeed: 1000000,
			Files: []slskd.File{
				{Filename: `Music\Saetia\Saetia - 1998\01.mp3`, Size: 100},
				{Filename: `Music\Saetia\Saetia - 1998\02.mp3`, Size: 101},
			},
		},
		{
			Username: "flacpeer", QueueLength: 5,
			Files: []slskd.File{
				{Filename: `SAETIA\SAETIA 1998\01 - vault.flac`, Size: 200},
				{Filename: `SAETIA\SAETIA 1998\02 - orbit.flac`, Size: 201},
			},
		},
	}
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateDownloading {
		t.Fatalf("state = %q, want downloading", want.State)
	}
	if want.Peer != "flacpeer" {
		t.Fatalf("peer = %q, want flacpeer (lossless beats free slot)", want.Peer)
	}
	if len(want.Enqueued) != 2 {
		t.Fatalf("enqueued = %d files, want 2", len(want.Enqueued))
	}
	enqueued := slskdFake.enqueued["flacpeer"]
	if len(enqueued) != 2 || enqueued[0].Size != 200 {
		t.Fatalf("enqueued at slskd = %+v", enqueued)
	}
}

func TestDownloadToImportHappyPath(t *testing.T) {
	slskdFake := newFakeSlskd()
	katydFake := autoTitlesResolver("vault")
	katydFake.importResult = importer.Result{Status: "imported", AlbumID: "saetia_saetia_1998"}
	orchestrator, root := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add(fetch.Spec{Artist: "saetia", Album: "saetia", Year: 0, GroupID: ""})
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	search := slskdFake.searches[want.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{{
		Username: "peer",
		Files:    []slskd.File{{Filename: `dir\SAETIA\01 - vault.flac`, Size: 500}},
	}}
	orchestrator.Tick(context.Background())

	downloads := filepath.Join(root, "downloads")
	localDir := filepath.Join(downloads, "SAETIA")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "01 - vault.flac"), make([]byte, 500), 0o644); err != nil {
		t.Fatal(err)
	}
	slskdFake.downloads = []slskd.UserResponse{{
		Username: "peer",
		Directories: []slskd.DirectoryResponse{{Files: []slskd.Transfer{{
			ID: "t1", Username: "peer", Filename: `dir\SAETIA\01 - vault.flac`, Size: 500,
			State: "Completed, Succeeded",
		}}}},
	}}
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateImported {
		t.Fatalf("state = %q error %q, want imported", want.State, want.Error)
	}
	if want.AlbumID != "saetia_saetia_1998" {
		t.Fatalf("album id = %q", want.AlbumID)
	}
	if len(katydFake.imported) != 1 || katydFake.imported[0].By != "fetchd" {
		t.Fatalf("imported requests = %+v", katydFake.imported)
	}
	if katydFake.imported[0].MBID != "r1" {
		t.Fatalf("import should pin the resolved release: %+v", katydFake.imported[0])
	}
	// the handoff is a staging dir of symlinks, never the downloads tree
	if got := katydFake.importDirFiles[0]; len(got) != 1 || got[0] != "01 - vault.flac" {
		t.Fatalf("staged files = %v", got)
	}
	if katydFake.imported[0].Dir == filepath.Join(root, "downloads", "SAETIA") {
		t.Fatalf("import should never see the downloads dir: %q", katydFake.imported[0].Dir)
	}
	if want.StagingDir != "" {
		t.Fatalf("staging dir should be cleaned after import: %q", want.StagingDir)
	}
	if _, err := os.Stat(filepath.Join(root, "downloads", "SAETIA", "01 - vault.flac")); !os.IsNotExist(err) {
		t.Fatalf("downloaded file should be purged after import: %v", err)
	}
	if err := orchestrator.Remove(want.ID); err != nil {
		t.Fatalf("remove terminal want: %v", err)
	}
	if _, ok := orchestrator.Want(want.ID); ok {
		t.Fatalf("want still present after remove")
	}
}

func TestImportNeedingDecisionPassthrough(t *testing.T) {
	slskdFake := newFakeSlskd()
	katydFake := autoTitlesResolver("vault")
	katydFake.importResult = importer.Result{Status: "needs_decision", Decision: &importer.Decision{
		Token: "tok123", Dir: "/x", Candidates: []match.Candidate{{ReleaseID: "r1"}},
	}}
	katydFake.decideResult = importer.Result{Status: "imported", AlbumID: "picked"}
	orchestrator, root := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add(fetch.Spec{Artist: "saetia", Album: "saetia", Year: 0, GroupID: ""})
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	search := slskdFake.searches[want.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{{
		Username: "peer",
		Files:    []slskd.File{{Filename: `dir\SAETIA\01 - vault.flac`, Size: 9}},
	}}
	orchestrator.Tick(context.Background())
	localDir := filepath.Join(root, "downloads", "SAETIA")
	os.MkdirAll(localDir, 0o755)
	os.WriteFile(filepath.Join(localDir, "01 - vault.flac"), make([]byte, 9), 0o644)
	slskdFake.downloads = []slskd.UserResponse{{
		Username: "peer",
		Directories: []slskd.DirectoryResponse{{Files: []slskd.Transfer{{
			ID: "t1", Filename: `dir\SAETIA\01 - vault.flac`, Size: 9, State: "Completed, Succeeded",
		}}}},
	}}
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateNeedsPick {
		t.Fatalf("state = %q, want needs_decision", want.State)
	}
	if want.DecisionToken != "tok123" {
		t.Fatalf("token = %q", want.DecisionToken)
	}
	if err := orchestrator.Remove(want.ID); err == nil {
		t.Fatalf("removing a non-terminal want must fail")
	}
	decided, err := orchestrator.Decide(context.Background(), want.ID, 1, false)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if decided.State != fetch.StateImported || decided.AlbumID != "picked" {
		t.Fatalf("decided = %+v", decided)
	}
	if len(katydFake.decided) != 1 || katydFake.decided[0] != "tok123" {
		t.Fatalf("katyd decide calls = %+v", katydFake.decided)
	}
}

func TestFailedTransfersFailTheWant(t *testing.T) {
	t.Skip("superseded: failures now re-enqueue; covered by TestFailedTransfersAreReenqueued")
	slskdFake := newFakeSlskd()
	katydFake := &fakeKatyd{
		resolve: api.ResolveResponse{
			Candidates: []match.Candidate{{ReleaseID: "r1", TrackCount: 2, TitleSim: 1, ArtistSim: 1}},
			Auto:       true,
		},
	}
	orchestrator, _ := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add(fetch.Spec{Artist: "saetia", Album: "saetia", Year: 0, GroupID: ""})
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	search := slskdFake.searches[want.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{{
		Username: "peer",
		Files: []slskd.File{
			{Filename: `dir\SAETIA\01 - vault.flac`, Size: 9},
			{Filename: `dir\SAETIA\02 - orbit.flac`, Size: 19},
		},
	}}
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	slskdFake.downloads = []slskd.UserResponse{{
		Username: "peer",
		Directories: []slskd.DirectoryResponse{{Files: []slskd.Transfer{
			{ID: "t1", Filename: `dir\SAETIA\01 - vault.flac`, Size: 9, State: "Completed, Succeeded"},
			{ID: "t2", Filename: `dir\SAETIA\02 - orbit.flac`, Size: 19, State: "Completed, Errored",
				Exception: strPtr("peer disconnected")},
		}}},
	}}
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateFailed {
		t.Fatalf("state = %q, want failed", want.State)
	}
	if want.Error == "" || len(katydFake.imported) != 0 {
		t.Fatalf("want = %+v, imports = %d", want, len(katydFake.imported))
	}
}

func TestNoResultsFailsTheWant(t *testing.T) {
	slskdFake := newFakeSlskd()
	katydFake := autoTitlesResolver("vault")
	orchestrator, _ := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add(fetch.Spec{Artist: "obscure", Album: "nothing", Year: 0, GroupID: ""})
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	slskdFake.searches[want.SearchID].IsComplete = true
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateFailed || want.Error == "" {
		t.Fatalf("want = %+v, want failed with error", want)
	}
}

func TestStoreRoundTripAndCorruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store, err := fetch.OpenStore(path)
	if err != nil {
		t.Fatalf("fresh store: %v", err)
	}
	orchestrator := fetch.New(store, fetch.Config{})
	if _, err := orchestrator.Add(fetch.Spec{Artist: "a", Album: "b", Year: 1999, GroupID: ""}); err != nil {
		t.Fatalf("add: %v", err)
	}
	reopened, err := fetch.OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if wants := reopened.Want.Wants; len(wants) != 1 || wants[0].Artist != "a" {
		t.Fatalf("reopened wants = %+v", wants)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fetch.OpenStore(path); err == nil {
		t.Fatalf("corrupt state must fail loud")
	}
}

func TestAddValidation(t *testing.T) {
	orchestrator, _ := harness(t, newFakeSlskd(), &fakeKatyd{})
	if _, err := orchestrator.Add(fetch.Spec{Artist: "", Album: "x", Year: 0, GroupID: ""}); err == nil {
		t.Fatalf("empty artist must fail")
	}
}

func strPtr(value string) *string { return &value }

func (f *fakeKatyd) RecordedResult(request string) (importer.Result, bool) {
	if f.recorded == nil {
		return importer.Result{}, false
	}
	result, ok := f.recorded[request]
	return result, ok
}

func TestWantsInNeedsPickReconcileExternalDecision(t *testing.T) {
	slskdFake := newFakeSlskd()
	katydFake := autoTitlesResolver("vault")
	katydFake.importResult = importer.Result{Status: "needs_decision", Decision: &importer.Decision{
		Token: "tok-1", Candidates: []match.Candidate{{ReleaseID: "r1"}},
	}}
	orchestrator, root := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add(fetch.Spec{Artist: "saetia", Album: "saetia", Year: 0, GroupID: ""})
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	search := slskdFake.searches[want.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{{
		Username: "peer",
		Files:    []slskd.File{{Filename: `dir\SAETIA\01 - vault.flac`, Size: 9}},
	}}
	orchestrator.Tick(context.Background())
	localDir := filepath.Join(root, "downloads", "SAETIA")
	os.MkdirAll(localDir, 0o755)
	os.WriteFile(filepath.Join(localDir, "01 - vault.flac"), make([]byte, 9), 0o644)
	slskdFake.downloads = []slskd.UserResponse{{
		Username: "peer",
		Directories: []slskd.DirectoryResponse{{Files: []slskd.Transfer{{
			ID: "t1", Filename: `dir\SAETIA\01 - vault.flac`, Size: 9, State: "Completed, Succeeded",
		}}}},
	}}
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateNeedsPick {
		t.Fatalf("state = %q, want needs_decision", want.State)
	}

	// resolved externally through kat decide: katyd recorded the outcome
	katydFake.recorded = map[string]importer.Result{
		want.ID: {Status: "imported", AlbumID: "saetia_saetia_1998"},
	}
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateImported || want.AlbumID != "saetia_saetia_1998" {
		t.Fatalf("state = %q album = %q, want imported", want.State, want.AlbumID)
	}
}

func TestFailedTransfersAreReenqueued(t *testing.T) {
	slskdFake := newFakeSlskd()
	katydFake := autoTitlesResolver("vault", "orbit")
	orchestrator, _ := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add(fetch.Spec{Artist: "saetia", Album: "saetia", Year: 0, GroupID: ""})
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	search := slskdFake.searches[want.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{{
		Username: "peer",
		Files: []slskd.File{
			{Filename: `dir\SAETIA\01 - vault.flac`, Size: 9},
			{Filename: `dir\SAETIA\02 - orbit.flac`, Size: 19},
		},
	}}
	orchestrator.Tick(context.Background())
	before := len(slskdFake.enqueuedSeq)
	slskdFake.downloads = []slskd.UserResponse{{
		Username: "peer",
		Directories: []slskd.DirectoryResponse{{Files: []slskd.Transfer{
			{ID: "t1", Filename: `dir\SAETIA\01 - vault.flac`, Size: 9, State: "Completed, Succeeded"},
			{ID: "t2", Filename: `dir\SAETIA\02 - orbit.flac`, Size: 19, State: "Completed, Errored",
				Exception: strPtr("Too many files")},
		}}},
	}}
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateDownloading {
		t.Fatalf("state = %q (%s), want still downloading while retrying", want.State, want.Error)
	}
	if len(slskdFake.enqueuedSeq) != before+1 {
		t.Fatalf("failed file should be re-enqueued: %d enqueues", len(slskdFake.enqueuedSeq)-before)
	}
	cumulative := slskdFake.enqueued["peer"]
	if len(cumulative) != 3 || cumulative[2].Filename != `dir\SAETIA\02 - orbit.flac` {
		t.Fatalf("last enqueue should be only the failed file: %+v", cumulative)
	}
}

// twoPeerWant starts a two-track want whose search answered from two
// peers, "peer" preferred (free slot) over "other", and returns it once
// the first peer has been enqueued.
func twoPeerWant(t *testing.T, slskdFake *fakeSlskd) (*fetch.Orchestrator, fetch.Want) {
	t.Helper()
	orchestrator, _ := harness(t, slskdFake, autoTitlesResolver("vault", "orbit"))
	want, _ := orchestrator.Add(fetch.Spec{Artist: "saetia", Album: "saetia", Year: 0, GroupID: ""})
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	search := slskdFake.searches[want.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{
		{Username: "peer", HasFreeUploadSlot: true, Files: []slskd.File{
			{Filename: `dir\SAETIA\01 - vault.flac`, Size: 9},
			{Filename: `dir\SAETIA\02 - orbit.flac`, Size: 19},
		}},
		{Username: "other", Files: []slskd.File{
			{Filename: `x\saetia\01 vault.flac`, Size: 10},
			{Filename: `x\saetia\02 orbit.flac`, Size: 20},
		}},
	}
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.Peer != "peer" {
		t.Fatalf("first pick = %q, want peer", want.Peer)
	}
	return orchestrator, want
}

func failedTransfers(user string, files map[string]int64, reason string) slskd.UserResponse {
	transfers := []slskd.Transfer{}
	for name, size := range files {
		transfers = append(transfers, slskd.Transfer{ID: name, Filename: name, Size: size, State: "Completed, Errored", Exception: strPtr(reason)})
	}
	return slskd.UserResponse{Username: user, Directories: []slskd.DirectoryResponse{{Files: transfers}}}
}

func TestWholeAlbumRefusedSwitchesPeer(t *testing.T) {
	slskdFake := newFakeSlskd()
	orchestrator, want := twoPeerWant(t, slskdFake)
	slskdFake.downloads = []slskd.UserResponse{failedTransfers("peer", map[string]int64{
		`dir\SAETIA\01 - vault.flac`: 9,
		`dir\SAETIA\02 - orbit.flac`: 19,
	}, "File not shared.")}

	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateSearching || len(want.ExcludedPeers) != 1 || want.ExcludedPeers[0] != "peer" {
		t.Fatalf("after refusal: state %q excluded %v err %q", want.State, want.ExcludedPeers, want.Error)
	}
	if len(slskdFake.searches) != 1 {
		t.Fatalf("the finished search should be reused, got %d searches", len(slskdFake.searches))
	}

	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateDownloading || want.Peer != "other" {
		t.Fatalf("should move to other: state %q peer %q err %q", want.State, want.Peer, want.Error)
	}
	if len(slskdFake.enqueued["other"]) != 2 {
		t.Fatalf("other enqueued %d files, want 2", len(slskdFake.enqueued["other"]))
	}
	if len(want.Attempts) != 0 {
		t.Fatalf("attempts should reset for the new peer: %v", want.Attempts)
	}
}

func TestRepeatedFileFailureSwitchesPeer(t *testing.T) {
	slskdFake := newFakeSlskd()
	orchestrator, want := twoPeerWant(t, slskdFake)
	slskdFake.downloads = []slskd.UserResponse{{Username: "peer", Directories: []slskd.DirectoryResponse{{Files: []slskd.Transfer{
		{ID: "t1", Filename: `dir\SAETIA\01 - vault.flac`, Size: 9, State: "Completed, Succeeded"},
		{ID: "t2", Filename: `dir\SAETIA\02 - orbit.flac`, Size: 19, State: "Completed, Rejected", Exception: strPtr("File not shared.")},
	}}}}}

	for i := 0; i < 3; i++ {
		orchestrator.Tick(context.Background())
		if got := find(t, orchestrator, want.ID); got.State != fetch.StateDownloading || got.Peer != "peer" {
			t.Fatalf("retry %d should stay on peer: state %q peer %q", i+1, got.State, got.Peer)
		}
	}
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateSearching || len(want.Excluded) != 1 || want.Excluded[`dir\SAETIA\02 - orbit.flac`] != "peer" {
		t.Fatalf("fourth failure should fail the slot over: state %q excluded %v err %q", want.State, want.Excluded, want.Error)
	}
	orchestrator.Tick(context.Background())
	got := find(t, orchestrator, want.ID)
	if got.State != fetch.StateDownloading || got.Owners[`x\saetia\02 orbit.flac`] != "other" {
		t.Fatalf("orbit should be re-sourced from other: state %q owners %v", got.State, got.Owners)
	}
}

func TestReenqueueErrorRetriesInsteadOfFailing(t *testing.T) {
	slskdFake := newFakeSlskd()
	orchestrator, _ := harness(t, slskdFake, autoTitlesResolver("vault", "orbit"))
	want, _ := orchestrator.Add(fetch.Spec{Artist: "saetia", Album: "saetia", Year: 0, GroupID: ""})
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	search := slskdFake.searches[want.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{{
		Username: "peer",
		Files: []slskd.File{
			{Filename: `dir\SAETIA\01 - vault.flac`, Size: 9},
			{Filename: `dir\SAETIA\02 - orbit.flac`, Size: 19},
		},
	}}
	orchestrator.Tick(context.Background())
	slskdFake.failEnqueue = map[string]error{
		"peer": errors.New(`POST /api/v0/transfers/downloads/peer: status 500: "Failed to connect to user peer"`),
	}
	slskdFake.downloads = []slskd.UserResponse{{Username: "peer", Directories: []slskd.DirectoryResponse{{Files: []slskd.Transfer{
		{ID: "t1", Filename: `dir\SAETIA\01 - vault.flac`, Size: 9, State: "Completed, Succeeded"},
		{ID: "t2", Filename: `dir\SAETIA\02 - orbit.flac`, Size: 19, State: "Completed, Errored",
			Exception: strPtr("Too many files")},
	}}}}}

	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateDownloading || want.Error != "" {
		t.Fatalf("re-enqueue error must not fail the want: state %q err %q", want.State, want.Error)
	}
	if !strings.Contains(strings.Join(want.Notes, "\n"), "will retry") {
		t.Fatalf("the failure should be noted for retry: %v", want.Notes)
	}

	for i := 0; i < 3; i++ {
		orchestrator.Tick(context.Background())
	}
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateSearching {
		t.Fatalf("the attempts bound should fail the file over to searching: state %q err %q", want.State, want.Error)
	}
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateFailed || !strings.Contains(want.Error, "no source left for 1 track(s)") {
		t.Fatalf("an unsourced slot with no peers left should fail loud: state %q err %q", want.State, want.Error)
	}
}

func TestInitialEnqueueErrorDropsPeerAndResources(t *testing.T) {
	slskdFake := newFakeSlskd()
	slskdFake.failEnqueue = map[string]error{"peer": errors.New(`status 500: "Failed to connect to user peer"`)}
	orchestrator, _ := harness(t, slskdFake, autoTitlesResolver("vault", "orbit"))
	want, _ := orchestrator.Add(fetch.Spec{Artist: "saetia", Album: "saetia", Year: 0, GroupID: ""})
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	search := slskdFake.searches[want.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{
		{Username: "peer", HasFreeUploadSlot: true, Files: []slskd.File{
			{Filename: `dir\SAETIA\01 - vault.flac`, Size: 9},
			{Filename: `dir\SAETIA\02 - orbit.flac`, Size: 19},
		}},
		{Username: "other", Files: []slskd.File{
			{Filename: `x\saetia\01 vault.flac`, Size: 10},
			{Filename: `x\saetia\02 orbit.flac`, Size: 20},
		}},
	}

	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateSearching || want.Error != "" {
		t.Fatalf("enqueue error must not fail the want: state %q err %q", want.State, want.Error)
	}
	if len(want.ExcludedPeers) != 1 || want.ExcludedPeers[0] != "peer" {
		t.Fatalf("the unreachable peer should be dropped: %v", want.ExcludedPeers)
	}
	if len(want.Enqueued) != 0 {
		t.Fatalf("a void pick should leave nothing enqueued: %v", want.Enqueued)
	}

	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateDownloading || want.Peer != "other" {
		t.Fatalf("should re-source from other: state %q peer %q err %q", want.State, want.Peer, want.Error)
	}
	if len(slskdFake.enqueued["other"]) != 2 {
		t.Fatalf("other should carry both files: %v", slskdFake.enqueued["other"])
	}
}

func TestFailoverEnqueueErrorKeepsSearching(t *testing.T) {
	slskdFake := newFakeSlskd()
	orchestrator, _ := harness(t, slskdFake, autoTitlesResolver("vault", "orbit"))
	want, _ := orchestrator.Add(fetch.Spec{Artist: "saetia", Album: "saetia", Year: 0, GroupID: ""})
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	search := slskdFake.searches[want.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{
		{Username: "peer", HasFreeUploadSlot: true, Files: []slskd.File{
			{Filename: `dir\SAETIA\01 - vault.flac`, Size: 9},
			{Filename: `dir\SAETIA\02 - orbit.flac`, Size: 19},
		}},
		{Username: "other", Files: []slskd.File{
			{Filename: `x\saetia\01 vault.flac`, Size: 10},
			{Filename: `x\saetia\02 orbit.flac`, Size: 20},
		}},
		{Username: "third", Files: []slskd.File{
			{Filename: `y\saetia\01 vault.flac`, Size: 11},
			{Filename: `y\saetia\02 orbit.flac`, Size: 21},
		}},
	}
	orchestrator.Tick(context.Background())
	slskdFake.downloads = []slskd.UserResponse{{Username: "peer", Directories: []slskd.DirectoryResponse{{Files: []slskd.Transfer{
		{ID: "t1", Filename: `dir\SAETIA\01 - vault.flac`, Size: 9, State: "Completed, Succeeded"},
		{ID: "t2", Filename: `dir\SAETIA\02 - orbit.flac`, Size: 19, State: "Completed, Errored",
			Exception: strPtr("Too many files")},
	}}}}}
	for i := 0; i < 4; i++ {
		orchestrator.Tick(context.Background())
	}
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateSearching {
		t.Fatalf("orbit should be searching for a new peer: state %q err %q", want.State, want.Error)
	}

	slskdFake.failEnqueue = map[string]error{"other": errors.New(`status 500: "Failed to connect to user other"`)}
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateSearching || want.Error != "" {
		t.Fatalf("a replacement enqueue error must not fail the want: state %q err %q", want.State, want.Error)
	}
	if len(want.ExcludedPeers) != 1 || want.ExcludedPeers[0] != "other" {
		t.Fatalf("the unreachable replacement peer should be dropped: %v", want.ExcludedPeers)
	}
	if want.Excluded[`dir\SAETIA\02 - orbit.flac`] != "peer" {
		t.Fatalf("the dead file should stay flagged for the next round: %v", want.Excluded)
	}

	slskdFake.failEnqueue = nil
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateDownloading || want.Owners[`y\saetia\02 orbit.flac`] != "third" {
		t.Fatalf("third should take the slot: state %q owners %v err %q", want.State, want.Owners, want.Error)
	}
}

func TestNoPeerLeftFailsTheWant(t *testing.T) {
	slskdFake := newFakeSlskd()
	orchestrator, want := twoPeerWant(t, slskdFake)
	refuse := func(user string, files map[string]int64) {
		slskdFake.downloads = []slskd.UserResponse{failedTransfers(user, files, "File not shared.")}
		orchestrator.Tick(context.Background())
		orchestrator.Tick(context.Background())
	}
	refuse("peer", map[string]int64{`dir\SAETIA\01 - vault.flac`: 9, `dir\SAETIA\02 - orbit.flac`: 19})
	refuse("other", map[string]int64{`x\saetia\01 vault.flac`: 10, `x\saetia\02 orbit.flac`: 20})

	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateFailed || !strings.Contains(want.Error, "no other peer") {
		t.Fatalf("state %q err %q, want failed with no other peer", want.State, want.Error)
	}
}

func autoTitlesResolver(titles ...string) *fakeKatyd {
	groups := []match.Candidate{{ReleaseID: "rg-1", TitleSim: 1, ArtistSim: 1}}
	releases := []match.Candidate{{ReleaseID: "r1", TrackCount: len(titles), TitleSim: 1, ArtistSim: 1, TrackTitles: titles}}
	return &fakeKatyd{
		resolve:         api.ResolveResponse{Candidates: groups, Auto: true},
		resolveGroup:    api.ResolveResponse{Candidates: releases, Auto: true},
		resolveGroupFor: "rg-1",
	}
}

func startDownloadWanted(t *testing.T, slskdFake *fakeSlskd, katydFake *fakeKatyd) (fetch.Want, string) {
	t.Helper()
	orchestrator, root := harness(t, slskdFake, katydFake)
	want, err := orchestrator.Add(fetch.Spec{Artist: "metallica", Album: "master of puppets", Year: 1986, GroupID: ""})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	orchestrator.Tick(context.Background())
	found := find(t, orchestrator, want.ID)
	search := slskdFake.searches[found.SearchID]
	search.IsComplete = true
	return want, root
}

func TestSingleIsRejectedForLowCoverage(t *testing.T) {
	slskdFake := newFakeSlskd()
	katydFake := autoTitlesResolver("Battery", "Master of Puppets", "The Thing That Should Not Be", "Orion")
	orchestrator, _ := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add(fetch.Spec{Artist: "metallica", Album: "master of puppets", Year: 1986, GroupID: ""})
	orchestrator.Tick(context.Background())
	found := find(t, orchestrator, want.ID)
	search := slskdFake.searches[found.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{{
		Username:          "singlepeer",
		HasFreeUploadSlot: true,
		Files: []slskd.File{
			{Filename: `MoP 7 inch\Master of Puppets.flac`, Size: 100},
			{Filename: `MoP 7 inch\Welcome Home (Sanitarium).flac`, Size: 101},
		},
	}}
	orchestrator.Tick(context.Background())
	found = find(t, orchestrator, want.ID)
	if found.State != fetch.StateFailed {
		t.Fatalf("state = %q (%s), want failed without downloading the single", found.State, found.Error)
	}
	if len(slskdFake.enqueuedSeq) != 0 {
		t.Fatalf("nothing should be enqueued: %+v", slskdFake.enqueuedSeq)
	}
}

func TestBestCoveragePeerWinsAndOnlyMatchedFilesEnqueue(t *testing.T) {
	slskdFake := newFakeSlskd()
	katydFake := autoTitlesResolver("Battery", "Master of Puppets", "The Thing That Should Not Be", "Orion")
	orchestrator, _ := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add(fetch.Spec{Artist: "metallica", Album: "master of puppets", Year: 1986, GroupID: ""})
	orchestrator.Tick(context.Background())
	found := find(t, orchestrator, want.ID)
	search := slskdFake.searches[found.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{
		{
			Username:          "singlepeer",
			HasFreeUploadSlot: true,
			Files: []slskd.File{
				{Filename: `single\Master of Puppets.flac`, Size: 100},
				{Filename: `single\Welcome Home (Sanitarium).flac`, Size: 101},
			},
		},
		{
			Username: "albumpeer",
			Files: []slskd.File{
				{Filename: `MoP\01 - Battery.flac`, Size: 200},
				{Filename: `MoP\02 - Master of Puppets.flac`, Size: 201},
				{Filename: `MoP\03 - The Thing That Should Not Be.flac`, Size: 202},
				{Filename: `MoP\04 - Orion.flac`, Size: 203},
			},
		},
	}
	orchestrator.Tick(context.Background())
	found = find(t, orchestrator, want.ID)
	if found.State != fetch.StateDownloading {
		t.Fatalf("state = %q (%s), want downloading", found.State, found.Error)
	}
	if found.Peer != "albumpeer" {
		t.Fatalf("peer = %q, want albumpeer", found.Peer)
	}
	if len(found.Enqueued) != 4 {
		t.Fatalf("enqueued %d files, want 4", len(found.Enqueued))
	}
}

func TestSplitMultiDiscDirectoriesPoolTogether(t *testing.T) {
	slskdFake := newFakeSlskd()
	katydFake := autoTitlesResolver("CD1 A", "CD1 B", "CD2 C", "CD2 D")
	orchestrator, _ := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add(fetch.Spec{Artist: "someartist", Album: "somealbum", Year: 0, GroupID: ""})
	orchestrator.Tick(context.Background())
	found := find(t, orchestrator, want.ID)
	search := slskdFake.searches[found.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{{
		Username: "splitpeer",
		Files: []slskd.File{
			{Filename: `Album\CD1\01 - CD1 A.flac`, Size: 100},
			{Filename: `Album\CD1\02 - CD1 B.flac`, Size: 101},
			{Filename: `Album\CD2\01 - CD2 C.flac`, Size: 102},
			{Filename: `Album\CD2\02 - CD2 D.flac`, Size: 103},
		},
	}}
	orchestrator.Tick(context.Background())
	found = find(t, orchestrator, want.ID)
	if found.State != fetch.StateDownloading || found.Peer != "splitpeer" {
		t.Fatalf("state = %q peer = %q (%s), want split directories pooled", found.State, found.Peer, found.Error)
	}
	if len(found.Enqueued) != 4 {
		t.Fatalf("enqueued %d, want 4", len(found.Enqueued))
	}
}

var blackPeerResponses = []slskd.Response{
	{Username: "peer", HasFreeUploadSlot: true, Files: []slskd.File{
		{Filename: `dir\SAETIA\01 - vault.flac`, Size: 9},
		{Filename: `dir\SAETIA\02 - orbit.flac`, Size: 19},
	}},
	{Username: "other", Files: []slskd.File{
		{Filename: `x\saetia\01 vault.flac`, Size: 10},
		{Filename: `x\saetia\02 orbit.flac`, Size: 20},
	}},
}

func blackRefusePeer(slskdFake *fakeSlskd) {
	slskdFake.downloads = []slskd.UserResponse{failedTransfers("peer", map[string]int64{
		`dir\SAETIA\01 - vault.flac`: 9,
		`dir\SAETIA\02 - orbit.flac`: 19,
	}, "File not shared.")}
}

func TestPeerBlacklistedAfterTwoDrops(t *testing.T) {
	slskdFake := newFakeSlskd()
	orchestrator, want := twoPeerWant(t, slskdFake)
	spec := fetch.Spec{Artist: "saetia", Album: "saetia", Year: 0, GroupID: ""}

	blackRefusePeer(slskdFake)
	orchestrator.Tick(context.Background())
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateDownloading || want.Peer != "other" {
		t.Fatalf("first drop: state %q peer %q err %q", want.State, want.Peer, want.Error)
	}

	queued, _ := orchestrator.Add(spec)
	orchestrator.Tick(context.Background())
	current := find(t, orchestrator, queued.ID)
	search := slskdFake.searches[current.SearchID]
	search.IsComplete = true
	search.Responses = blackPeerResponses
	orchestrator.Tick(context.Background())
	current = find(t, orchestrator, queued.ID)
	if current.Peer != "peer" {
		t.Fatalf("one strike should not blacklist yet: peer %q", current.Peer)
	}
	blackRefusePeer(slskdFake)
	orchestrator.Tick(context.Background())
	orchestrator.Tick(context.Background())
	current = find(t, orchestrator, queued.ID)
	if current.State != fetch.StateDownloading || current.Peer != "other" {
		t.Fatalf("second drop: state %q peer %q err %q", current.State, current.Peer, current.Error)
	}

	queued2, _ := orchestrator.Add(spec)
	orchestrator.Tick(context.Background())
	current2 := find(t, orchestrator, queued2.ID)
	search2 := slskdFake.searches[current2.SearchID]
	search2.IsComplete = true
	search2.Responses = blackPeerResponses
	orchestrator.Tick(context.Background())
	current2 = find(t, orchestrator, queued2.ID)
	if current2.Peer != "other" {
		t.Fatalf("blacklisted peer must be skipped despite the free slot: peer %q", current2.Peer)
	}
	if got := len(slskdFake.enqueued["peer"]); got != 4 {
		t.Fatalf("blacklisted peer must not be enqueued again: %d files", got)
	}
}

func TestBlacklistYieldsAndForgivesOnDelivery(t *testing.T) {
	slskdFake := newFakeSlskd()
	orchestrator, want := twoPeerWant(t, slskdFake)
	spec := fetch.Spec{Artist: "saetia", Album: "saetia", Year: 0, GroupID: ""}

	blackRefusePeer(slskdFake)
	orchestrator.Tick(context.Background())
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	if want.State != fetch.StateDownloading || want.Peer != "other" {
		t.Fatalf("first drop: state %q peer %q err %q", want.State, want.Peer, want.Error)
	}

	// second want whose search offers ONLY the struck peer: still
	// allowed (one strike), and the refusal blacklists it
	queued, _ := orchestrator.Add(spec)
	orchestrator.Tick(context.Background())
	current := find(t, orchestrator, queued.ID)
	search := slskdFake.searches[current.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{blackPeerResponses[0]}
	orchestrator.Tick(context.Background())
	current = find(t, orchestrator, queued.ID)
	if current.Peer != "peer" {
		t.Fatalf("one strike should not blacklist yet: peer %q", current.Peer)
	}
	blackRefusePeer(slskdFake)
	orchestrator.Tick(context.Background())
	orchestrator.Tick(context.Background())
	current = find(t, orchestrator, queued.ID)
	if current.Peer != "peer" || current.State != fetch.StateDownloading {
		t.Fatalf("starvation should yield to the blacklisted peer: state %q peer %q err %q", current.State, current.Peer, current.Error)
	}
	if !strings.Contains(strings.Join(current.Notes, "\n"), "blacklist yielded") {
		t.Fatalf("the yield should be noted: %v", current.Notes)
	}

	// a delivered file forgives the peer
	slskdFake.downloads = []slskd.UserResponse{{Username: "peer", Directories: []slskd.DirectoryResponse{{Files: []slskd.Transfer{
		{ID: "t1", Filename: `dir\SAETIA\01 - vault.flac`, Size: 9, State: "Completed, Succeeded"},
		{ID: "t2", Filename: `dir\SAETIA\02 - orbit.flac`, Size: 19, State: "Completed, Errored",
			Exception: strPtr("Too many files")},
	}}}}}
	orchestrator.Tick(context.Background())
	current = find(t, orchestrator, queued.ID)
	if current.State != fetch.StateDownloading {
		t.Fatalf("delivery should forgive and keep downloading: state %q err %q", current.State, current.Error)
	}

	queued2, _ := orchestrator.Add(spec)
	orchestrator.Tick(context.Background())
	current2 := find(t, orchestrator, queued2.ID)
	search2 := slskdFake.searches[current2.SearchID]
	search2.IsComplete = true
	search2.Responses = blackPeerResponses
	orchestrator.Tick(context.Background())
	current2 = find(t, orchestrator, queued2.ID)
	if current2.Peer != "peer" {
		t.Fatalf("a forgiven peer should be pickable again: peer %q", current2.Peer)
	}
}

func (f *fakeKatyd) ResolveGroup(group string) (api.ResolveResponse, error) {
	if len(f.resolveGroup.Candidates) > 0 && group == f.resolveGroupFor {
		return f.resolveGroup, nil
	}
	return api.ResolveResponse{}, errors.New("no such group fixture")
}
