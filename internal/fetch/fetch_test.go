package fetch_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"doppel.moe/katydid/internal/api"
	"doppel.moe/katydid/internal/fetch"
	"doppel.moe/katydid/internal/importer"
	"doppel.moe/katydid/internal/match"
	"doppel.moe/katydid/internal/slskd"
)

type fakeSlskd struct {
	searches    map[string]*slskd.Search
	enqueued    map[string][]slskd.File
	downloads   []slskd.UserResponse
	createErr   error
	enqueuedSeq []string
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
	f.enqueued[username] = append(f.enqueued[username], files...)
	f.enqueuedSeq = append(f.enqueuedSeq, username)
	return nil
}

func (f *fakeSlskd) Downloads(_ context.Context) ([]slskd.UserResponse, error) {
	return f.downloads, nil
}

type fakeKatyd struct {
	resolve      api.ResolveResponse
	resolveErr   error
	imported     []importer.Request
	importResult importer.Result
	decided      []string
	decideResult importer.Result
	recorded     map[string]importer.Result
}

func (f *fakeKatyd) Resolve(_ string, _ string, _ int, _ string) (api.ResolveResponse, error) {
	return f.resolve, f.resolveErr
}

func (f *fakeKatyd) Import(req importer.Request) (importer.Result, error) {
	f.imported = append(f.imported, req)
	return f.importResult, nil
}

func (f *fakeKatyd) Decide(token string, _ importer.DecideInput) (importer.Result, error) {
	f.decided = append(f.decided, token)
	return f.decideResult, nil
}

func harness(t *testing.T, slskdFake *fakeSlskd, katydFake *fakeKatyd) (*fetch.Orchestrator, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := fetch.OpenStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	orchestrator := fetch.New(store, fetch.Config{
		Slskd:        slskdFake,
		Katyd:        katydFake,
		DownloadsDir: filepath.Join(dir, "downloads"),
	})
	return orchestrator, dir
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
	katydFake := &fakeKatyd{resolve: api.ResolveResponse{
		Candidates: []match.Candidate{{ReleaseID: "r1", TrackCount: 2, TitleSim: 1, ArtistSim: 1}},
		Auto:       true,
	}}
	orchestrator, _ := harness(t, slskdFake, katydFake)
	want, err := orchestrator.Add("saetia", "saetia", 1998, "")
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

func TestLowConfidenceGatesOnReleasePick(t *testing.T) {
	slskdFake := newFakeSlskd()
	katydFake := &fakeKatyd{resolve: api.ResolveResponse{
		Candidates: []match.Candidate{
			{ReleaseID: "r1", TrackCount: 10, TitleSim: 0.7, ArtistSim: 1},
			{ReleaseID: "r2", TrackCount: 12, TitleSim: 0.65, ArtistSim: 0.9},
		},
		Auto: false,
	}}
	orchestrator, _ := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add("drukqs", "drukqs", 2001, "")
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
	katydFake := &fakeKatyd{resolve: api.ResolveResponse{
		Candidates: []match.Candidate{{ReleaseID: "r1", TrackCount: 2, TitleSim: 1, ArtistSim: 1}},
		Auto:       true,
	}}
	orchestrator, _ := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add("saetia", "saetia", 0, "")
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
	katydFake := &fakeKatyd{
		resolve: api.ResolveResponse{
			Candidates: []match.Candidate{{ReleaseID: "r1", TrackCount: 1, TitleSim: 1, ArtistSim: 1}},
			Auto:       true,
		},
		importResult: importer.Result{Status: "imported", AlbumID: "saetia_saetia_1998"},
	}
	orchestrator, root := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add("saetia", "saetia", 0, "")
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
	if err := orchestrator.Remove(want.ID); err != nil {
		t.Fatalf("remove terminal want: %v", err)
	}
	if _, ok := orchestrator.Want(want.ID); ok {
		t.Fatalf("want still present after remove")
	}
}

func TestImportNeedingDecisionPassthrough(t *testing.T) {
	slskdFake := newFakeSlskd()
	katydFake := &fakeKatyd{
		resolve: api.ResolveResponse{
			Candidates: []match.Candidate{{ReleaseID: "r1", TrackCount: 1, TitleSim: 1, ArtistSim: 1}},
			Auto:       true,
		},
		importResult: importer.Result{Status: "needs_decision", Decision: &importer.Decision{
			Token: "tok123", Dir: "/x", Candidates: []match.Candidate{{ReleaseID: "r1"}},
		}},
		decideResult: importer.Result{Status: "imported", AlbumID: "picked"},
	}
	orchestrator, root := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add("saetia", "saetia", 0, "")
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	search := slskdFake.searches[want.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{{
		Username: "peer",
		Files:    []slskd.File{{Filename: `dir\SAETIA\01.flac`, Size: 9}},
	}}
	orchestrator.Tick(context.Background())
	localDir := filepath.Join(root, "downloads", "SAETIA")
	os.MkdirAll(localDir, 0o755)
	os.WriteFile(filepath.Join(localDir, "01.flac"), make([]byte, 9), 0o644)
	slskdFake.downloads = []slskd.UserResponse{{
		Username: "peer",
		Directories: []slskd.DirectoryResponse{{Files: []slskd.Transfer{{
			ID: "t1", Filename: `dir\SAETIA\01.flac`, Size: 9, State: "Completed, Succeeded",
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
	want, _ := orchestrator.Add("saetia", "saetia", 0, "")
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	search := slskdFake.searches[want.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{{
		Username: "peer",
		Files: []slskd.File{
			{Filename: `dir\SAETIA\01.flac`, Size: 9},
			{Filename: `dir\SAETIA\02.flac`, Size: 19},
		},
	}}
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	slskdFake.downloads = []slskd.UserResponse{{
		Username: "peer",
		Directories: []slskd.DirectoryResponse{{Files: []slskd.Transfer{
			{ID: "t1", Filename: `dir\SAETIA\01.flac`, Size: 9, State: "Completed, Succeeded"},
			{ID: "t2", Filename: `dir\SAETIA\02.flac`, Size: 19, State: "Completed, Errored",
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
	katydFake := &fakeKatyd{resolve: api.ResolveResponse{
		Candidates: []match.Candidate{{ReleaseID: "r1", TrackCount: 1, TitleSim: 1, ArtistSim: 1}},
		Auto:       true,
	}}
	orchestrator, _ := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add("obscure", "nothing", 0, "")
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
	if _, err := orchestrator.Add("a", "b", 1999, ""); err != nil {
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
	if _, err := orchestrator.Add("", "x", 0, ""); err == nil {
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
	katydFake := &fakeKatyd{
		resolve: api.ResolveResponse{
			Candidates: []match.Candidate{{ReleaseID: "r1", TrackCount: 1, TitleSim: 1, ArtistSim: 1}},
			Auto:       true,
		},
		importResult: importer.Result{Status: "needs_decision", Decision: &importer.Decision{
			Token: "tok-1", Candidates: []match.Candidate{{ReleaseID: "r1"}},
		}},
	}
	orchestrator, root := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add("saetia", "saetia", 0, "")
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	search := slskdFake.searches[want.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{{
		Username: "peer",
		Files:    []slskd.File{{Filename: `dir\SAETIA\01.flac`, Size: 9}},
	}}
	orchestrator.Tick(context.Background())
	localDir := filepath.Join(root, "downloads", "SAETIA")
	os.MkdirAll(localDir, 0o755)
	os.WriteFile(filepath.Join(localDir, "01.flac"), make([]byte, 9), 0o644)
	slskdFake.downloads = []slskd.UserResponse{{
		Username: "peer",
		Directories: []slskd.DirectoryResponse{{Files: []slskd.Transfer{{
			ID: "t1", Filename: `dir\SAETIA\01.flac`, Size: 9, State: "Completed, Succeeded",
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
	katydFake := &fakeKatyd{
		resolve: api.ResolveResponse{
			Candidates: []match.Candidate{{ReleaseID: "r1", TrackCount: 2, TitleSim: 1, ArtistSim: 1}},
			Auto:       true,
		},
	}
	orchestrator, _ := harness(t, slskdFake, katydFake)
	want, _ := orchestrator.Add("saetia", "saetia", 0, "")
	orchestrator.Tick(context.Background())
	want = find(t, orchestrator, want.ID)
	search := slskdFake.searches[want.SearchID]
	search.IsComplete = true
	search.Responses = []slskd.Response{{
		Username: "peer",
		Files: []slskd.File{
			{Filename: `dir\SAETIA\01.flac`, Size: 9},
			{Filename: `dir\SAETIA\02.flac`, Size: 19},
		},
	}}
	orchestrator.Tick(context.Background())
	before := len(slskdFake.enqueuedSeq)
	slskdFake.downloads = []slskd.UserResponse{{
		Username: "peer",
		Directories: []slskd.DirectoryResponse{{Files: []slskd.Transfer{
			{ID: "t1", Filename: `dir\SAETIA\01.flac`, Size: 9, State: "Completed, Succeeded"},
			{ID: "t2", Filename: `dir\SAETIA\02.flac`, Size: 19, State: "Completed, Errored",
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
	if len(cumulative) != 3 || cumulative[2].Filename != `dir\SAETIA\02.flac` {
		t.Fatalf("last enqueue should be only the failed file: %+v", cumulative)
	}
}

func autoTitlesResolver(titles ...string) *fakeKatyd {
	candidates := []match.Candidate{{ReleaseID: "r1", TrackCount: len(titles), TitleSim: 1, ArtistSim: 1, TrackTitles: titles}}
	return &fakeKatyd{resolve: api.ResolveResponse{Candidates: candidates, Auto: true}}
}

func startDownloadWanted(t *testing.T, slskdFake *fakeSlskd, katydFake *fakeKatyd) (fetch.Want, string) {
	t.Helper()
	orchestrator, root := harness(t, slskdFake, katydFake)
	want, err := orchestrator.Add("metallica", "master of puppets", 1986, "")
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
	want, _ := orchestrator.Add("metallica", "master of puppets", 1986, "")
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
	want, _ := orchestrator.Add("metallica", "master of puppets", 1986, "")
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
	want, _ := orchestrator.Add("someartist", "somealbum", 0, "")
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
