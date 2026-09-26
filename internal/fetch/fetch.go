// Package fetch drives slskd downloads into katydid imports.
package fetch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"doppel.moe/katydid/internal/api"
	"doppel.moe/katydid/internal/importer"
	"doppel.moe/katydid/internal/library"
	"doppel.moe/katydid/internal/match"
	"doppel.moe/katydid/internal/slskd"
)

const schema = 1

const (
	StateQueued       = "queued"
	StateNeedsRelease = "needs_release"
	StateSearching    = "searching"
	StateDownloading  = "downloading"
	StateNeedsPick    = "needs_decision"
	StateImported     = "imported"
	StateSkipped      = "skipped"
	StateFailed       = "failed"
)

var audioExtensions = map[string]bool{
	".flac": true, ".mp3": true, ".m4a": true, ".ogg": true,
	".opus": true, ".wav": true, ".wma": true, ".aiff": true, ".aif": true,
}

// staleAfter bounds how long a want may sit in searching or downloading
// without observable progress before the orchestrator fails it.
const staleAfter = 6 * time.Hour

type Want struct {
	ID          string   `json:"id"`
	Artist      string   `json:"artist"`
	Album       string   `json:"album"`
	Year        int      `json:"year,omitempty"`
	MBID        string   `json:"mbid,omitempty"`
	TrackCount  int      `json:"track_count,omitempty"`
	TrackTitles []string `json:"track_titles,omitempty"`
	GroupID     string   `json:"group_id,omitempty"`
	// ReleaseArtist and ReleaseTitle are the musicbrainz names once
	// resolved; Artist and Album stay as typed, since soulseek folders
	// often match the typed (romanized) spelling better
	ReleaseArtist string    `json:"release_artist,omitempty"`
	ReleaseTitle  string    `json:"release_title,omitempty"`
	ReleaseID     string    `json:"release_id,omitempty"`
	State         string    `json:"state"`
	Error         string    `json:"error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`

	Candidates    []match.Candidate `json:"candidates,omitempty"`
	SearchID      string            `json:"search_id,omitempty"`
	Peer          string            `json:"peer,omitempty"`
	RemoteDir     string            `json:"remote_dir,omitempty"`
	Enqueued      []slskd.File      `json:"enqueued,omitempty"`
	Downloaded    int               `json:"downloaded,omitempty"`
	DecisionToken string            `json:"decision_token,omitempty"`
	ExcludedPeers []string          `json:"excluded_peers,omitempty"`
	AlbumID       string            `json:"album_id,omitempty"`
	Attempts      map[string]int    `json:"attempts,omitempty"`
	Notes         []string          `json:"notes,omitempty"`
}

// Katyd is the slice of the katyd client the orchestrator needs.
type Katyd interface {
	Resolve(artist, album string, year int, mbid string) (api.ResolveResponse, error)
	ResolveGroup(group string) (api.ResolveResponse, error)
	Import(req importer.Request) (importer.Result, error)
	Albums(query library.Query) (api.AlbumsResponse, error)
	Decide(token string, in importer.DecideInput) (importer.Result, error)
	RecordedResult(request string) (importer.Result, bool)
}

// Slskd is the slice of the slskd client the orchestrator needs.
type Slskd interface {
	CreateSearch(ctx context.Context, req slskd.SearchRequest) (*slskd.Search, error)
	Search(ctx context.Context, id string, includeResponses bool) (*slskd.Search, error)
	DeleteSearch(ctx context.Context, id string) error
	EnqueueDownloads(ctx context.Context, username string, files []slskd.File) error
	Downloads(ctx context.Context) ([]slskd.UserResponse, error)
}

type Config struct {
	Slskd        Slskd
	Katyd        Katyd
	DownloadsDir string
}

// Store persists wants as a single atomically written JSON document; the
// queue is small and wants change together, so one file keeps consistency
// trivial. It is truth, not a cache.
type Store struct {
	path string
	mu   sync.Mutex
	Want struct {
		Schema int    `json:"schema"`
		Wants  []Want `json:"wants"`
	}
}

func OpenStore(path string) (*Store, error) {
	store := &Store{path: path}
	store.Want.Schema = schema
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, &store.Want); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	if store.Want.Schema != schema {
		return nil, fmt.Errorf("state %s has schema %d, want %d", path, store.Want.Schema, schema)
	}
	return store, nil
}

func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	encoded, err := json.MarshalIndent(s.Want, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	temp := s.path + ".tmp"
	if err := os.WriteFile(temp, encoded, 0o644); err != nil {
		return fmt.Errorf("write state %s: %w", temp, err)
	}
	if err := os.Rename(temp, s.path); err != nil {
		return fmt.Errorf("replace state %s: %w", s.path, err)
	}
	return nil
}

func (s *Store) snapshot() []Want {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Want, len(s.Want.Wants))
	copy(out, s.Want.Wants)
	return out
}

func (s *Store) byID(id string) (Want, bool) {
	for _, want := range s.Want.Wants {
		if want.ID == id {
			return want, true
		}
	}
	return Want{}, false
}

func (s *Store) put(updated Want) {
	for i, want := range s.Want.Wants {
		if want.ID == updated.ID {
			updated.UpdatedAt = time.Now().UTC()
			s.Want.Wants[i] = updated
			return
		}
	}
}

type Orchestrator struct {
	store *Store
	cfg   Config
	mu    sync.Mutex
}

func New(store *Store, cfg Config) *Orchestrator {
	return &Orchestrator{store: store, cfg: cfg}
}

// Spec queues what to fetch: the typed specifier as entered, plus the
// musicbrainz names and a pinned group or release when the caller (the
// bot) already resolved them.
type Spec struct {
	Artist        string
	Album         string
	Year          int
	MBID          string
	GroupID       string
	ReleaseArtist string
	ReleaseTitle  string
}

// Add records a new want. A non-empty GroupID pins the release group and
// skips the group search. Resolution happens on the next tick.
func (o *Orchestrator) Add(spec Spec) (Want, error) {
	if spec.Artist == "" || spec.Album == "" {
		return Want{}, errors.New("artist and album are required")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now().UTC()
	want := Want{
		ID:            newID(),
		Artist:        spec.Artist,
		Album:         spec.Album,
		Year:          spec.Year,
		MBID:          spec.MBID,
		GroupID:       spec.GroupID,
		ReleaseArtist: spec.ReleaseArtist,
		ReleaseTitle:  spec.ReleaseTitle,
		State:         StateQueued,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	o.store.Want.Wants = append(o.store.Want.Wants, want)
	if err := o.store.Save(); err != nil {
		return Want{}, err
	}
	return want, nil
}

// Wants returns all wants, oldest first.
func (o *Orchestrator) Wants() []Want {
	return o.store.snapshot()
}

// Want returns one want by id.
func (o *Orchestrator) Want(id string) (Want, bool) {
	o.store.mu.Lock()
	defer o.store.mu.Unlock()
	want, ok := o.store.byID(id)
	return want, ok
}

// Decide resolves either a needs_release want (pick into its stored
// candidates) or a needs_decision want (passthrough to katyd).
func (o *Orchestrator) Decide(ctx context.Context, id string, pick int, skip bool) (Want, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	want, ok := o.store.byID(id)
	if !ok {
		return Want{}, fmt.Errorf("no want with id %s", id)
	}
	switch want.State {
	case StateNeedsRelease:
		if skip {
			want.State = StateSkipped
			o.store.put(want)
			return want, o.store.Save()
		}
		if pick < 1 || pick > len(want.Candidates) {
			return Want{}, fmt.Errorf("pick %d out of range 1..%d", pick, len(want.Candidates))
		}
		// the picked candidate is a release group; resolve it to its
		// oldest release before searching
		want.GroupID = want.Candidates[pick-1].ReleaseID
		want.Candidates = nil
		if err := o.applyGroupRelease(ctx, &want); err != nil {
			want.State = StateFailed
			want.Error = err.Error()
		}
		o.store.put(want)
		if err := o.store.Save(); err != nil {
			return Want{}, err
		}
		return want, nil
	case StateNeedsPick:
		result, err := o.cfg.Katyd.Decide(want.DecisionToken, importer.DecideInput{Pick: pick, Skip: skip})
		if err != nil {
			return Want{}, err
		}
		return o.absorbResultLocked(want, result)
	default:
		return Want{}, fmt.Errorf("want %s is %s, not awaiting a decision", id, want.State)
	}
}

// Remove deletes a terminal want. Non-terminal wants must be decided first.
func (o *Orchestrator) Remove(id string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	want, ok := o.store.byID(id)
	if !ok {
		return fmt.Errorf("no want with id %s", id)
	}
	switch want.State {
	case StateImported, StateSkipped, StateFailed:
	default:
		return fmt.Errorf("want %s is %s; only terminal wants can be removed", id, want.State)
	}
	kept := o.store.Want.Wants[:0]
	for _, candidate := range o.store.Want.Wants {
		if candidate.ID != id {
			kept = append(kept, candidate)
		}
	}
	o.store.Want.Wants = kept
	return o.store.Save()
}

// Tick advances every active want by one step. The daemon calls it on a
// ticker; tests call it directly.
func (o *Orchestrator) Tick(ctx context.Context) {
	for _, want := range o.store.snapshot() {
		switch want.State {
		case StateQueued:
			o.resolve(ctx, want)
		case StateSearching:
			o.pollSearch(ctx, want)
		case StateDownloading:
			o.pollTransfers(ctx, want)
		case StateNeedsPick:
			o.reconcileDecision(ctx, want)
		case StateNeedsRelease, StateImported, StateSkipped, StateFailed:
		}
	}
}

func (o *Orchestrator) withWant(want Want, mutate func(*Want) error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := mutate(&want); err != nil {
		want.State = StateFailed
		want.Error = err.Error()
		o.stopSearch(&want)
	}
	o.store.put(want)
	if err := o.store.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "fetchd: persist state: %v\n", err)
	}
}

func (o *Orchestrator) resolve(ctx context.Context, want Want) {
	if want.MBID == "" && want.GroupID != "" {
		// the group is pinned; the group search is skipped entirely
		o.withWant(want, func(w *Want) error {
			return o.applyGroupRelease(ctx, w)
		})
		return
	}
	response, err := o.cfg.Katyd.Resolve(want.Artist, want.Album, want.Year, want.MBID)
	o.withWant(want, func(w *Want) error {
		if err != nil {
			return fmt.Errorf("resolve: %w", err)
		}
		if len(response.Candidates) == 0 {
			return errors.New("no musicbrainz candidates")
		}
		if want.MBID != "" {
			// an exact release was requested; nothing to disambiguate
			w.GroupID = response.Candidates[0].GroupID
			w.ReleaseArtist, w.ReleaseTitle = response.Candidates[0].Artist, response.Candidates[0].Title
			w.ReleaseID = want.MBID
			w.TrackCount = response.Candidates[0].TrackCount
			w.TrackTitles = response.Candidates[0].TrackTitles
			w.Candidates = nil
			if o.alreadyOwned(w) {
				w.State = StateImported
				return nil
			}
			return o.beginSearch(ctx, w)
		}
		if response.Auto {
			// the top release group is trusted; default to its oldest
			// release (remasters and deluxes want an explicit mbid)
			w.GroupID = response.Candidates[0].ReleaseID
			return o.applyGroupRelease(ctx, w)
		}
		w.Candidates = response.Candidates
		w.State = StateNeedsRelease
		return nil
	})
}

// applyGroupRelease resolves the chosen release group to its oldest
// release and stores its shape for the source picker.
func (o *Orchestrator) applyGroupRelease(ctx context.Context, w *Want) error {
	releases, err := o.cfg.Katyd.ResolveGroup(w.GroupID)
	if err != nil {
		return fmt.Errorf("resolve group: %w", err)
	}
	if len(releases.Candidates) == 0 {
		return fmt.Errorf("release group %s has no releases", w.GroupID)
	}
	w.ReleaseArtist, w.ReleaseTitle = releases.Candidates[0].Artist, releases.Candidates[0].Title
	w.ReleaseID = releases.Candidates[0].ReleaseID
	w.TrackCount = releases.Candidates[0].TrackCount
	w.TrackTitles = releases.Candidates[0].TrackTitles
	w.Candidates = nil
	if o.alreadyOwned(w) {
		w.State = StateImported
		return nil
	}
	return o.beginSearch(ctx, w)
}

// alreadyOwned short-circuits a want whose release is already in the
// library: re-downloading the same release id adds nothing. A failed
// library listing fails open and fetches as usual. The share link is
// still minted by the bot, which looks the album up by release id in
// navidrome.
func (o *Orchestrator) alreadyOwned(w *Want) bool {
	if w.ReleaseID == "" {
		return false
	}
	albums, err := o.cfg.Katyd.Albums(library.Query{})
	if err != nil {
		return false
	}
	for _, album := range albums.Albums {
		if !strings.EqualFold(album.Meta.ReleaseID, w.ReleaseID) {
			continue
		}
		w.AlbumID = album.ID
		w.Notes = append(w.Notes, "already in the library as "+album.ID+"; nothing downloaded")
		return true
	}
	return false
}

func (o *Orchestrator) beginSearchLocked(ctx context.Context, want Want) (Want, error) {
	err := o.beginSearch(ctx, &want)
	if err != nil {
		want.State = StateFailed
		want.Error = err.Error()
	}
	o.store.put(want)
	if err == nil {
		err = o.store.Save()
	} else {
		_ = o.store.Save()
	}
	return want, err
}

func (o *Orchestrator) beginSearch(ctx context.Context, want *Want) error {
	search, err := o.cfg.Slskd.CreateSearch(ctx, slskd.SearchRequest{
		SearchText: want.Artist + " " + want.Album,
		FileLimit:  500,
	})
	if err != nil {
		return fmt.Errorf("create search: %w", err)
	}
	want.SearchID = search.ID
	want.State = StateSearching
	return nil
}

func (o *Orchestrator) pollSearch(ctx context.Context, want Want) {
	search, err := o.cfg.Slskd.Search(ctx, want.SearchID, true)
	o.withWant(want, func(w *Want) error {
		if err != nil {
			return fmt.Errorf("poll search: %w", err)
		}
		if !search.IsComplete {
			if time.Since(w.UpdatedAt) > staleAfter {
				return errors.New("search stale")
			}
			return nil
		}
		peer, files, err := pickSource(search, w.TrackTitles, w.TrackCount, w.ExcludedPeers)
		if err != nil {
			if len(w.ExcludedPeers) > 0 {
				return fmt.Errorf("no other peer after dropping %s: %w", strings.Join(w.ExcludedPeers, ", "), err)
			}
			return err
		}
		if err := o.cfg.Slskd.EnqueueDownloads(ctx, peer, files); err != nil {
			return fmt.Errorf("enqueue %d files from %s: %w", len(files), peer, err)
		}
		w.Peer = peer
		w.RemoteDir = remoteParent(files[0].Filename)
		w.Enqueued = files
		w.State = StateDownloading
		return nil
	})
}

// maxDownloadRetries bounds how often a failed transfer is re-enqueued
// before the peer is dropped; maxPeers bounds how many peers a want
// tries before it fails.
const (
	maxDownloadRetries = 3
	maxPeers           = 5
)

func (o *Orchestrator) pollTransfers(ctx context.Context, want Want) {
	users, err := o.cfg.Slskd.Downloads(ctx)
	o.withWant(want, func(w *Want) error {
		if err != nil {
			return fmt.Errorf("poll transfers: %w", err)
		}
		var transfers []slskd.Transfer
		for _, user := range users {
			if user.Username != w.Peer {
				continue
			}
			for _, directory := range user.Directories {
				transfers = append(transfers, directory.Files...)
			}
		}
		enqueued := map[string]int64{}
		filesByName := map[string]slskd.File{}
		for _, file := range w.Enqueued {
			enqueued[file.Filename] = file.Size
			filesByName[file.Filename] = file
		}
		byName := map[string]slskd.Transfer{}
		for _, transfer := range transfers {
			if _, wanted := enqueued[transfer.Filename]; !wanted {
				continue
			}
			// re-enqueued files appear twice; the newest entry wins
			if existing, ok := byName[transfer.Filename]; ok && existing.RequestedAt.After(transfer.RequestedAt.Time) {
				continue
			}
			byName[transfer.Filename] = transfer
		}
		if len(byName) < len(enqueued) {
			if time.Since(w.UpdatedAt) > staleAfter {
				return fmt.Errorf("only %d of %d transfers visible after %s", len(byName), len(enqueued), staleAfter)
			}
			return nil
		}
		succeeded := 0
		failed := []slskd.Transfer{}
		for name := range enqueued {
			transfer := byName[name]
			switch {
			case slskd.TransferSucceeded(transfer.State):
				succeeded++
				delete(w.Attempts, name)
			case slskd.TransferFailed(transfer.State):
				failed = append(failed, transfer)
			}
		}
		w.Downloaded = succeeded
		if len(failed) > 0 {
			if w.Attempts == nil {
				w.Attempts = map[string]int{}
			}
			exhausted := false
			retry := []slskd.File{}
			for _, transfer := range failed {
				w.Attempts[transfer.Filename]++
				if w.Attempts[transfer.Filename] > maxDownloadRetries {
					exhausted = true
				}
				retry = append(retry, filesByName[transfer.Filename])
			}
			summary := failureSummary(failed, len(enqueued))
			// a peer refusing the whole album at once is blocking it, and a
			// file that keeps failing will not come from this peer either
			if exhausted || len(failed) == len(enqueued) {
				return o.switchPeer(ctx, w, summary)
			}
			if err := o.cfg.Slskd.EnqueueDownloads(ctx, w.Peer, retry); err != nil {
				return fmt.Errorf("re-enqueue %d failed files: %w", len(retry), err)
			}
			w.Notes = append(w.Notes, "re-enqueued "+summary)
			return nil
		}
		if succeeded < len(enqueued) {
			return nil
		}
		dir, err := locateDownload(w, o.cfg.DownloadsDir)
		if err != nil {
			return err
		}
		result, err := o.cfg.Katyd.Import(importer.Request{
			Dir:     dir,
			By:      "fetchd",
			Request: w.ID,
		})
		if err != nil {
			return fmt.Errorf("import %s: %w", dir, err)
		}
		return o.absorbResult(w, result)
	})
}

// switchPeer drops the current peer and goes back to source picking,
// reusing the finished search when slskd still has it.
func (o *Orchestrator) switchPeer(ctx context.Context, w *Want, reason string) error {
	dropped := w.Peer
	w.ExcludedPeers = append(w.ExcludedPeers, dropped)
	w.Notes = append(w.Notes, fmt.Sprintf("dropped peer %s: %s", dropped, reason))
	w.Peer, w.RemoteDir, w.Enqueued, w.Attempts, w.Downloaded = "", "", nil, nil, 0
	if len(w.ExcludedPeers) >= maxPeers {
		return fmt.Errorf("gave up after %d peers, last %s: %s", len(w.ExcludedPeers), dropped, reason)
	}
	if w.SearchID != "" {
		if _, err := o.cfg.Slskd.Search(ctx, w.SearchID, false); err == nil {
			w.State = StateSearching
			return nil
		}
	}
	return o.beginSearch(ctx, w)
}

// failureSummary condenses failed transfers by reason: "3 of 12 transfers
// failed: File not shared. x3".
func failureSummary(failed []slskd.Transfer, total int) string {
	counts := map[string]int{}
	for _, transfer := range failed {
		reason := "unknown reason"
		if transfer.Exception != nil && *transfer.Exception != "" {
			reason = *transfer.Exception
		}
		counts[reason]++
	}
	reasons := make([]string, 0, len(counts))
	for reason := range counts {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	parts := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		parts = append(parts, fmt.Sprintf("%s x%d", reason, counts[reason]))
	}
	return fmt.Sprintf("%d of %d transfers failed: %s", len(failed), total, strings.Join(parts, ", "))
}

// reconcileDecision picks up decisions resolved through another client;
// the kat cli talks to katyd directly, and the outcome is recorded under
// the want id.
func (o *Orchestrator) reconcileDecision(ctx context.Context, want Want) {
	result, found := o.cfg.Katyd.RecordedResult(want.ID)
	if !found {
		return
	}
	o.withWant(want, func(w *Want) error {
		if err := o.absorbResult(w, result); err != nil {
			return err
		}
		w.Notes = append(w.Notes, "decision resolved outside fetchd")
		w.DecisionToken = ""
		return nil
	})
}

// locateDownload maps enqueued remote files to their local directory by
// scanning the slskd downloads tree for basename+size matches, so no slskd
// destination configuration is required. Ambiguity fails loud.
func locateDownload(want *Want, downloadsDir string) (string, error) {
	wantFiles := map[string]int64{}
	for _, file := range want.Enqueued {
		wantFiles[strings.ToLower(filepath.Base(remoteName(file.Filename)))] = file.Size
	}
	matches := map[string]int{}
	err := filepath.WalkDir(downloadsDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		size, wanted := wantFiles[strings.ToLower(entry.Name())]
		if !wanted {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() != size {
			return nil
		}
		matches[filepath.Dir(path)]++
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("scan downloads %s: %w", downloadsDir, err)
	}
	best, bestCount := "", 0
	ties := []string{}
	for dir, count := range matches {
		if count > bestCount {
			best, bestCount = dir, count
			ties = nil
		} else if count == bestCount {
			ties = append(ties, dir)
		}
	}
	if bestCount == 0 {
		return "", fmt.Errorf("no downloaded files found under %s", downloadsDir)
	}
	if len(ties) > 0 || bestCount < len(wantFiles) {
		return "", fmt.Errorf("ambiguous download location: %d of %d files in %s", bestCount, len(wantFiles), best)
	}
	return best, nil
}

// pickSource chooses the peer directory that best matches the release.
// minCoverage is the fraction of the release track list a peer's files
// must plausibly cover before anything is downloaded; below it the want
// fails instead of pulling partial or tribute junk.
const minCoverage = 0.6

// fileTitleGuess extracts a comparable title from a soulseek filename:
// extension and leading track numbers stripped.
func fileTitleGuess(name string) string {
	base := remoteName(name)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	dash := strings.Index(base, " - ")
	if dash >= 0 {
		base = base[dash+3:]
	}
	trimmed := strings.TrimLeft(base, "0123456789 .-_")
	if trimmed != "" {
		base = trimmed
	}
	return base
}

// coverage is how well a peer's files cover a release track list: each
// file is greedily matched to an unclaimed title and only reasonably
// close matches count.
func coverageOf(files []slskd.File, titles []string) (float64, []slskd.File) {
	used := make([]bool, len(titles))
	matched := []slskd.File{}
	for _, file := range files {
		guess := strings.ToLower(fileTitleGuess(file.Filename))
		best, bestSim := -1, 0.0
		for i, title := range titles {
			if used[i] {
				continue
			}
			if sim := match.Similarity(guess, strings.ToLower(title)); sim > bestSim {
				best, bestSim = i, sim
			}
		}
		if best >= 0 && bestSim >= 0.5 {
			used[best] = true
			matched = append(matched, file)
		}
	}
	if len(titles) == 0 {
		return 0, nil
	}
	return float64(len(matched)) / float64(len(titles)), matched
}

// pickSource chooses the peer whose files best cover the release track
// list (falling back to count heuristics when titles are unknown) and
// returns only the files that plausibly belong to the release.
func pickSource(search *slskd.Search, titles []string, trackCount int, excluded []string) (string, []slskd.File, error) {
	skip := map[string]bool{}
	for _, peer := range excluded {
		skip[peer] = true
	}
	type offer struct {
		peer     string
		score    float64
		coverage float64
		files    []slskd.File
	}
	var offers []offer
	for _, response := range search.Responses {
		if skip[response.Username] {
			continue
		}
		files := []slskd.File{}
		for _, file := range response.Files {
			if file.IsLocked || !audioExtensions[strings.ToLower(filepath.Ext(remoteName(file.Filename)))] {
				continue
			}
			files = append(files, file)
		}
		if len(files) == 0 {
			continue
		}
		choice := offer{peer: response.Username, files: files}
		if len(titles) > 0 {
			choice.coverage, choice.files = coverageOf(files, titles)
			choice.score = choice.coverage
		} else if trackCount > 0 {
			delta := len(files) - trackCount
			if delta < 0 {
				delta = -delta
			}
			choice.coverage = float64(len(files)) / float64(trackCount)
			choice.score = 1.0 - float64(delta)/float64(trackCount)
		} else {
			choice.coverage = 1
			choice.score = 0.5
		}
		lossless := 0
		for _, file := range choice.files {
			if strings.EqualFold(filepath.Ext(remoteName(file.Filename)), ".flac") {
				lossless++
			}
		}
		if len(choice.files) > 0 {
			choice.score += 0.2 * float64(lossless) / float64(len(choice.files))
		}
		if response.HasFreeUploadSlot {
			choice.score += 0.1
		}
		if response.QueueLength < 100 {
			choice.score += 0.05
		}
		offers = append(offers, choice)
	}
	if len(offers) == 0 {
		return "", nil, errors.New("no usable audio files in any response")
	}
	sort.Slice(offers, func(i, j int) bool {
		if offers[i].score != offers[j].score {
			return offers[i].score > offers[j].score
		}
		return offers[i].peer < offers[j].peer
	})
	best := offers[0]
	if len(titles) > 0 && best.coverage < minCoverage {
		return "", nil, fmt.Errorf("best peer %s covers only %d%% of the %d release tracks; nothing downloaded", best.peer, int(best.coverage*100), len(titles))
	}
	sort.Slice(best.files, func(i, j int) bool {
		return remoteName(best.files[i].Filename) < remoteName(best.files[j].Filename)
	})
	return best.peer, best.files, nil
}

// remoteParent and remoteName handle soulseek backslash paths portably.
func remoteParent(path string) string {
	if index := strings.LastIndexAny(path, `/\`); index >= 0 {
		return path[:index]
	}
	return path
}

func remoteName(path string) string {
	if index := strings.LastIndexAny(path, `/\`); index >= 0 {
		return path[index+1:]
	}
	return path
}

func (o *Orchestrator) absorbResultLocked(want Want, result importer.Result) (Want, error) {
	err := o.absorbResult(&want, result)
	o.store.put(want)
	if saveErr := o.store.Save(); err == nil && saveErr != nil {
		err = saveErr
	}
	return want, err
}

func (o *Orchestrator) absorbResult(want *Want, result importer.Result) error {
	switch result.Status {
	case "imported":
		want.State = StateImported
		want.AlbumID = result.AlbumID
		want.DecisionToken = ""
	case "needs_decision":
		if result.Decision == nil {
			return errors.New("katyd returned needs_decision without a token")
		}
		want.State = StateNeedsPick
		want.DecisionToken = result.Decision.Token
	case "skipped":
		want.State = StateSkipped
	default:
		return fmt.Errorf("unexpected import status %q", result.Status)
	}
	return nil
}

func (o *Orchestrator) stopSearch(want *Want) {
	if want.SearchID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = o.cfg.Slskd.DeleteSearch(ctx, want.SearchID)
	want.SearchID = ""
}

func newID() string {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		panic("fetch: rand: " + err.Error())
	}
	return hex.EncodeToString(raw)
}
