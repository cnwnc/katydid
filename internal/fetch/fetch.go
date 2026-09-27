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
	ReleaseArtist string `json:"release_artist,omitempty"`
	ReleaseTitle  string `json:"release_title,omitempty"`
	ReleaseID     string `json:"release_id,omitempty"`
	// Source and SourceURL mark non-musicbrainz imports: source is
	// "lastfm" and everything downstream marks the artifacts unvetted
	Source    string    `json:"source,omitempty"`
	SourceURL string    `json:"source_url,omitempty"`
	State     string    `json:"state"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Candidates []match.Candidate `json:"candidates,omitempty"`
	SearchID   string            `json:"search_id,omitempty"`
	Peer       string            `json:"peer,omitempty"`
	RemoteDir  string            `json:"remote_dir,omitempty"`
	Enqueued   []slskd.File      `json:"enqueued,omitempty"`
	// Paths binds each enqueued remote file to its local location once
	// the transfer is seen finished; StagingDir is the assembled handoff
	// directory (symlinks) katyd imports from.
	Paths         map[string]string `json:"paths,omitempty"`
	StagingDir    string            `json:"staging_dir,omitempty"`
	Downloaded    int               `json:"downloaded,omitempty"`
	DecisionToken string            `json:"decision_token,omitempty"`
	ExcludedPeers []string          `json:"excluded_peers,omitempty"`
	// Owners maps each enqueued remote file to the peer it is fetched
	// from: single-host wants carry one owner for everything, pooled
	// wants split the album across peers. Slots maps each file to its
	// track index so failover can re-source one slot without re-picking
	// the album. Attempts and Excluded track failures per file, so a
	// dead (file, peer) pair hands that one slot to another peer.
	Owners    map[string]string `json:"owners,omitempty"`
	Slots     map[string]int    `json:"slots,omitempty"`
	Excluded  map[string]string `json:"excluded,omitempty"`
	BindSince time.Time         `json:"bind_since,omitempty"`
	AlbumID   string            `json:"album_id,omitempty"`
	Attempts  map[string]int    `json:"attempts,omitempty"`
	Notes     []string          `json:"notes,omitempty"`
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
	// BindRetry bounds how long a want waits for downloaded files to
	// turn up on disk before failing; zero means staleAfter.
	BindRetry time.Duration
	// ExpireNeedsDecision and ExpireTerminal bound how long parked
	// decisions and finished wants (and their downloads) linger; zero
	// means the package defaults.
	ExpireNeedsDecision time.Duration
	ExpireTerminal      time.Duration
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
	// Source, SourceURL and TrackTitles seed a non-musicbrainz want:
	// source "lastfm" skips resolution and hunts soulseek directly
	Source      string
	SourceURL   string
	TrackTitles []string
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
		ReleaseID:     spec.MBID,
		Source:        spec.Source,
		SourceURL:     spec.SourceURL,
		TrackTitles:   spec.TrackTitles,
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
			continue
		}
		dropStaging(&candidate)
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
	o.reap()
}

// reap times out parked decisions and deletes finished wants together
// with their downloads: on a tmpfs downloads tree the files are ram,
// so nothing may linger.
func (o *Orchestrator) reap() {
	decision, terminal := o.cfg.ExpireNeedsDecision, o.cfg.ExpireTerminal
	if decision == 0 {
		decision = defaultExpireNeedsDecision
	}
	if terminal == 0 {
		terminal = defaultExpireTerminal
	}
	for _, want := range o.store.snapshot() {
		switch want.State {
		case StateNeedsPick:
			if time.Since(want.UpdatedAt) <= decision {
				continue
			}
			o.withWant(want, func(w *Want) error {
				w.State = StateFailed
				w.Error = fmt.Sprintf("needs a decision, expired after %s (token %s); re-add with /addalbum", decision, want.DecisionToken)
				o.purgeDownloads(w)
				return nil
			})
		case StateImported, StateSkipped, StateFailed:
			if time.Since(want.UpdatedAt) <= terminal {
				continue
			}
			_ = o.Remove(want.ID)
		}
	}
}

const (
	defaultExpireNeedsDecision = 24 * time.Hour
	defaultExpireTerminal      = 2 * time.Hour
)

func (o *Orchestrator) withWant(want Want, mutate func(*Want) error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := mutate(&want); err != nil {
		want.State = StateFailed
		want.Error = err.Error()
		o.stopSearch(&want)
		o.purgeDownloads(&want)
	}
	o.store.put(want)
	if err := o.store.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "fetchd: persist state: %v\n", err)
	}
}

func (o *Orchestrator) resolve(ctx context.Context, want Want) {
	if want.Source == sourceLastFM {
		// identity came from last.fm; names are authoritative and the
		// search can start immediately
		o.withWant(want, func(w *Want) error {
			return o.beginSearch(ctx, w)
		})
		return
	}
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
	// a last.fm want searches by the authoritative names; the typed
	// specifier may be the misspelling the lookup corrected
	artist, album := want.Artist, want.Album
	if want.Source == sourceLastFM && want.ReleaseArtist != "" && want.ReleaseTitle != "" {
		artist, album = want.ReleaseArtist, want.ReleaseTitle
	}
	search, err := o.cfg.Slskd.CreateSearch(ctx, slskd.SearchRequest{
		SearchText: artist + " " + album,
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
		var pick pickAssignments
		if len(w.Enqueued) == 0 {
			fresh, err := pickSource(search, w.TrackTitles, w.TrackCount, w.ExcludedPeers)
			if err != nil {
				if len(w.ExcludedPeers) > 0 {
					return fmt.Errorf("no other peer after dropping %s: %w", strings.Join(w.ExcludedPeers, ", "), err)
				}
				return err
			}
			pick = fresh
		} else {
			// failover round: only the slots whose (file, peer) pair died
			// need re-sourcing; everything bound or healthy is untouched
			var err error
			pick, err = failoverPlan(w, search)
			if err != nil {
				return err
			}
			if len(pick.plan) == 0 {
				return fmt.Errorf("no source left for %d track(s) after peers failed", len(w.Enqueued)-len(w.Paths))
			}
		}
		// grouped per owner: a pool enqueues one batch per peer
		batches := map[string][]slskd.File{}
		for _, file := range pick.plan {
			batches[pick.owners[file.Filename]] = append(batches[pick.owners[file.Filename]], file)
		}
		for peer, files := range batches {
			if err := o.cfg.Slskd.EnqueueDownloads(ctx, peer, files); err != nil {
				return fmt.Errorf("enqueue %d files from %s: %w", len(files), peer, err)
			}
		}
		if len(w.Enqueued) == 0 {
			w.Owners = pick.owners
			w.Slots = pick.slots
			w.Enqueued = pick.plan
		} else {
			// swap dead files for their re-sourced replacements; their
			// stale owner/slot entries go with them
			live := map[string]bool{}
			for _, file := range w.Enqueued {
				if _, dead := w.Excluded[file.Filename]; !dead {
					live[file.Filename] = true
				}
			}
			kept := w.Enqueued[:0]
			for _, file := range w.Enqueued {
				if live[file.Filename] {
					kept = append(kept, file)
				} else {
					delete(w.Owners, file.Filename)
					delete(w.Slots, file.Filename)
				}
			}
			for _, file := range pick.plan {
				kept = append(kept, file)
				w.Owners[file.Filename] = pick.owners[file.Filename]
				w.Slots[file.Filename] = pick.slots[file.Filename]
			}
			w.Enqueued = kept
		}
		// display peer: the first owner
		w.Peer = ""
		for _, file := range w.Enqueued {
			w.Peer = w.Owners[file.Filename]
			break
		}
		w.RemoteDir = remoteParent(w.Enqueued[0].Filename)
		w.State = StateDownloading
		// pooling is worth one note at plan time only; failover churn
		// already notes each moved file
		if len(w.Enqueued) == len(pick.plan) && len(batches) > 1 {
			w.Notes = append(w.Notes, fmt.Sprintf("pooling %d peers, ~%d tracks each", len(batches), len(pick.plan)/len(batches)))
		}
		return nil
	})
}

// failoverPlan re-sources the want's dead slots from the finished
// search: for each enqueued file marked dead in Excluded, find another
// peer offering the same track index, preferring peers already in the
// pool and refusing lossy substitutes for a flac slot.
func failoverPlan(w *Want, search *slskd.Search) (pickAssignments, error) {
	type candidate struct {
		peer string
		file slskd.File
	}
	perIndex := map[int][]candidate{}
	for _, response := range search.Responses {
		excludedPeer := false
		for _, peer := range w.ExcludedPeers {
			if peer == response.Username {
				excludedPeer = true
				break
			}
		}
		if excludedPeer {
			continue
		}
		for _, file := range response.Files {
			if file.IsLocked || !audioExtensions[strings.ToLower(filepath.Ext(remoteName(file.Filename)))] {
				continue
			}
			idx := fileIndex(file, w.TrackTitles)
			if idx < 0 {
				continue
			}
			perIndex[idx] = append(perIndex[idx], candidate{peer: response.Username, file: file})
		}
	}
	plan := pickAssignments{owners: map[string]string{}, slots: map[string]int{}}
	for _, file := range w.Enqueued {
		deadPeer, dead := w.Excluded[file.Filename]
		if !dead {
			continue
		}
		index, known := w.Slots[file.Filename]
		if !known {
			return plan, fmt.Errorf("no recorded slot for %s; cannot fail over", remoteName(file.Filename))
		}
		options := []candidate{}
		flac := []candidate{}
		for _, c := range perIndex[index] {
			if c.peer == deadPeer {
				continue
			}
			options = append(options, c)
			if strings.EqualFold(filepath.Ext(remoteName(c.file.Filename)), ".flac") {
				flac = append(flac, c)
			}
		}
		// flac slots stay flac: a lossy stand-in breaks the pool purity
		if len(flac) > 0 {
			options = flac
		}
		if len(options) == 0 {
			continue
		}
		// prefer the peer already carrying the most live slots of this
		// want: lane reuse beats opening new ones
		load := map[string]int{}
		for _, live := range w.Enqueued {
			if owner, ok := w.Owners[live.Filename]; ok && !liveFileDead(w, live.Filename) {
				load[owner]++
			}
		}
		chosen := options[0]
		for _, c := range options[1:] {
			if load[c.peer] > load[chosen.peer] || (load[c.peer] == load[chosen.peer] && c.peer < chosen.peer) {
				chosen = c
			}
		}
		plan.plan = append(plan.plan, chosen.file)
		plan.owners[chosen.file.Filename] = chosen.peer
		plan.slots[chosen.file.Filename] = index
	}
	if len(plan.plan) == 0 {
		return plan, nil
	}
	sort.Slice(plan.plan, func(i, j int) bool {
		return remoteName(plan.plan[i].Filename) < remoteName(plan.plan[j].Filename)
	})
	return plan, nil
}

// maxDownloadRetries bounds how often a failed transfer is re-enqueued
// before the peer is dropped; maxPeers bounds how many peers a want
// tries before it fails.
const (
	maxDownloadRetries = 3
	maxPeers           = 5
)

// sourceLastFM marks wants whose identity comes from last.fm instead of
// musicbrainz.
const sourceLastFM = "lastfm"

func (o *Orchestrator) pollTransfers(ctx context.Context, want Want) {
	users, err := o.cfg.Slskd.Downloads(ctx)
	o.withWant(want, func(w *Want) error {
		if err != nil {
			return fmt.Errorf("poll transfers: %w", err)
		}
		// transfers are matched per owning peer: in a pool, peer A's
		// copy of a filename has nothing to do with peer B's
		byOwner := map[string]map[string]slskd.Transfer{}
		for _, user := range users {
			for _, directory := range user.Directories {
				for _, transfer := range directory.Files {
					owner := w.Owners[transfer.Filename]
					if owner == "" {
						owner = w.Peer
					}
					if owner != user.Username {
						continue
					}
					if byOwner[owner] == nil {
						byOwner[owner] = map[string]slskd.Transfer{}
					}
					// re-enqueued files appear twice; the newest entry wins
					if existing, ok := byOwner[owner][transfer.Filename]; ok && existing.RequestedAt.After(transfer.RequestedAt.Time) {
						continue
					}
					byOwner[owner][transfer.Filename] = transfer
				}
			}
		}
		failed := []slskd.Transfer{}
		for _, file := range w.Enqueued {
			owner := w.Owners[file.Filename]
			if owner == "" {
				owner = w.Peer
			}
			transfer, visible := byOwner[owner][file.Filename]
			if !visible {
				continue
			}
			switch {
			case slskd.TransferSucceeded(transfer.State):
				delete(w.Attempts, file.Filename)
			case slskd.TransferFailed(transfer.State):
				// a file already bound from disk is done, whatever the
				// transfer list still claims about it
				if _, bound := w.Paths[file.Filename]; !bound {
					failed = append(failed, transfer)
				}
			}
		}
		// disk truth first: files on disk bind and import even when
		// slskd's queue is jammed, was cleared, or still reports errors
		if err := o.bindPaths(w); err != nil {
			return err
		}
		w.Downloaded = len(w.Paths)
		if w.StagingDir == "" && len(failed) > 0 {
			// a whole-album refusal with nothing staged is a blocked
			// peer: switch immediately instead of re-asking it
			wholeRefusal := len(w.Paths) == 0 && len(failed) == len(w.Enqueued)
			oneOwner := true
			for _, transfer := range failed {
				owner := w.Owners[transfer.Filename]
				if owner == "" {
					owner = w.Peer
				}
				if owner != w.Owners[failed[0].Filename] && w.Owners[failed[0].Filename] != "" {
					oneOwner = false
					break
				}
			}
			if wholeRefusal && oneOwner {
				summary := failureSummary(failed, len(w.Enqueued))
				return o.switchPeer(ctx, w, summary)
			}
			if w.Attempts == nil {
				w.Attempts = map[string]int{}
			}
			retry := []slskd.File{}
			perFile := []string{}
			for _, transfer := range failed {
				w.Attempts[transfer.Filename]++
				file := slskd.File{Filename: transfer.Filename, Size: transfer.Size}
				if attempts := w.Attempts[transfer.Filename]; attempts > maxDownloadRetries {
					// per-file failover: mark this file off-limits for
					// its owner and let the next search round hand it to
					// another peer; the rest of the pool is untouched
					owner := w.Owners[transfer.Filename]
					if owner == "" {
						owner = w.Peer
					}
					if w.Excluded == nil {
						w.Excluded = map[string]string{}
					}
					if w.Excluded[transfer.Filename] == "" {
						w.Excluded[transfer.Filename] = owner
						w.Notes = append(w.Notes, fmt.Sprintf("file %s given up on %s: %s", remoteName(transfer.Filename), owner, failureReason(transfer)))
					}
					continue
				}
				retry = append(retry, file)
				perFile = append(perFile, fmt.Sprintf("%s: %s", remoteName(transfer.Filename), failureReason(transfer)))
			}
			if len(retry) > 0 {
				batches := map[string][]slskd.File{}
				for _, file := range retry {
					owner := w.Owners[file.Filename]
					if owner == "" {
						owner = w.Peer
					}
					batches[owner] = append(batches[owner], file)
				}
				for peer, files := range batches {
					if err := o.cfg.Slskd.EnqueueDownloads(ctx, peer, files); err != nil {
						return fmt.Errorf("re-enqueue %d failed files: %w", len(files), err)
					}
				}
				w.Notes = append(w.Notes, "re-enqueued "+strings.Join(perFile, "; "))
			}
			if len(w.Excluded) >= maxPeers {
				// every slot has burned through its peers
				missing := len(w.Enqueued) - len(w.Paths)
				return fmt.Errorf("gave up after %d file(s) exhausted their peers; %d track(s) unsourced", len(w.Excluded), missing)
			}
			if len(retry) == 0 && len(w.Excluded) > 0 {
				// some slots need a new peer: back to the search for a
				// failover round (bound files are untouched)
				w.State = StateSearching
				return nil
			}
			return nil
		}
		if w.StagingDir == "" {
			return nil
		}
		req := importer.Request{
			Dir:     w.StagingDir,
			By:      "fetchd",
			Request: w.ID,
		}
		if w.Source == sourceLastFM {
			// the importer re-looks the album up on last.fm; the names
			// must be the exact ones the want was confirmed with
			req.Source = w.Source
			req.Artist = w.ReleaseArtist
			req.Album = w.ReleaseTitle
		} else if w.ReleaseID != "" {
			// the release was resolved and the files were picked against
			// its track list, so import must not re-guess identity from
			// soulseek file tags
			req.MBID = w.ReleaseID
		}
		result, err := o.cfg.Katyd.Import(req)
		if err != nil {
			return fmt.Errorf("import %s: %w", w.StagingDir, err)
		}
		return o.absorbResult(w, result)
	})
}

// switchPeer drops the current peer and goes back to source picking,
// reusing the finished search when slskd still has it. A want with
// staged files never switches: switching re-picks a different peer's
// directory and the staged files are bound to slots already, so the
// want holds instead and keeps retrying the rest on this peer.
func (o *Orchestrator) switchPeer(ctx context.Context, w *Want, reason string) error {
	if len(w.Paths) > 0 {
		w.Notes = append(w.Notes, fmt.Sprintf("peer %s failed but %d file(s) already staged; holding", w.Peer, len(w.Paths)))
		return nil
	}
	dropped := w.Peer
	w.ExcludedPeers = append(w.ExcludedPeers, dropped)
	w.Notes = append(w.Notes, fmt.Sprintf("dropped peer %s: %s", dropped, reason))
	o.purgeDownloads(w)
	w.Peer, w.RemoteDir, w.Enqueued, w.Attempts, w.Downloaded = "", "", nil, nil, 0
	w.BindSince = time.Time{}
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

// liveFileDead reports whether a want's file is marked for failover.
func liveFileDead(w *Want, filename string) bool {
	_, dead := w.Excluded[filename]
	return dead
}

// failureReason extracts a transfer's exception, defaulting sensibly.
func failureReason(transfer slskd.Transfer) string {
	if transfer.Exception != nil && *transfer.Exception != "" {
		return *transfer.Exception
	}
	return "unknown reason"
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

// bindPaths resolves the local path of every enqueued file: slots stay
// bound to the specific path they were found at, so a later re-download
// for a different peer cannot mix copies. Ambiguous matches fail loud;
// missing files retry until the bind window closes, since slskd can
// mark a transfer complete a beat before the file is settled.
func (o *Orchestrator) bindPaths(w *Want) error {
	// bindings made before a reboot (tmpfs wipes the tree) dangle;
	// drop them so the slots re-bind or fail by name
	for name, path := range w.Paths {
		if _, err := os.Stat(path); err != nil {
			delete(w.Paths, name)
			w.Notes = append(w.Notes, "downloaded file vanished: "+filepath.Base(path)+"; rebinding")
		}
	}
	missing := []string{}
	ambiguous := []string{}
	found := map[string]string{}
	for _, file := range w.Enqueued {
		name := strings.ToLower(filepath.Base(remoteName(file.Filename)))
		if _, bound := w.Paths[file.Filename]; bound {
			continue
		}
		var matches []string
		err := filepath.WalkDir(o.cfg.DownloadsDir, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				// a not-yet-existing downloads tree holds nothing; that
				// is the normal state before any download lands
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if entry.IsDir() || !strings.EqualFold(entry.Name(), name) {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Size() == file.Size {
				matches = append(matches, path)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("scan downloads %s: %w", o.cfg.DownloadsDir, err)
		}
		switch {
		case len(matches) == 1:
			found[file.Filename] = matches[0]
		case len(matches) > 1:
			ambiguous = append(ambiguous, fmt.Sprintf("%s found at %s and %s", name, matches[0], matches[1]))
		default:
			missing = append(missing, fmt.Sprintf("%s (%d bytes)", name, file.Size))
		}
	}
	if len(ambiguous) > 0 {
		return fmt.Errorf("duplicate downloads, delete the stale copy: %s", strings.Join(ambiguous, "; "))
	}
	// found slots bind immediately and permanently, even while others
	// are still in flight
	if len(found) > 0 {
		if w.Paths == nil {
			w.Paths = map[string]string{}
		}
		for name, path := range found {
			w.Paths[name] = path
		}
	}
	if len(missing) > 0 {
		retry := o.cfg.BindRetry
		if retry == 0 {
			retry = staleAfter
		}
		// the window anchors on the first missing observation: UpdatedAt
		// refreshes every poll and would never let it close
		if w.BindSince.IsZero() {
			w.BindSince = time.Now().UTC()
			return nil
		}
		if time.Since(w.BindSince) > retry {
			return fmt.Errorf("%d of %d downloaded files never turned up on disk after %s: %s", len(missing), len(w.Enqueued), retry, strings.Join(missing, ", "))
		}
		return nil
	}
	w.BindSince = time.Time{}
	if len(w.Paths) < len(w.Enqueued) {
		return nil
	}
	stage, err := stageFiles(w.Paths, w.Enqueued)
	if err != nil {
		return err
	}
	w.StagingDir = stage
	return nil
}

// stageFiles assembles the handoff directory: one symlink per bound
// file, katyd follows them when it copies into the library. Basename
// collisions (multi-disc reuses) get a numeric infix like slskd's own.
func stageFiles(paths map[string]string, enqueued []slskd.File) (string, error) {
	stage, err := os.MkdirTemp("", "katydid-fetchd-*")
	if err != nil {
		return "", fmt.Errorf("create staging dir: %w", err)
	}
	used := map[string]bool{}
	for _, file := range enqueued {
		base := filepath.Base(remoteName(file.Filename))
		number := 1
		for used[base] {
			ext := filepath.Ext(base)
			stem := strings.TrimSuffix(base, ext)
			base = fmt.Sprintf("%s.%d%s", stem, number, ext)
			number++
		}
		used[base] = true
		if err := os.Symlink(paths[file.Filename], filepath.Join(stage, base)); err != nil {
			os.RemoveAll(stage)
			return "", fmt.Errorf("stage %s: %w", base, err)
		}
	}
	return stage, nil
}

// dropStaging removes a want's handoff directory once nothing needs it.
func dropStaging(w *Want) {
	if w.StagingDir != "" {
		os.RemoveAll(w.StagingDir)
		w.StagingDir = ""
	}
}

// purgeDownloads deletes the files a finished want downloaded, any
// download directories it emptied, and its staging dir. Files still
// referenced by an active want survive, and only paths this want bound
// are ever touched — never a directory wholesale, because slskd users
// may keep unrelated downloads there.
func (o *Orchestrator) purgeDownloads(w *Want) {
	shared := map[string]bool{}
	for _, other := range o.store.snapshot() {
		if other.ID == w.ID {
			continue
		}
		switch other.State {
		case StateImported, StateSkipped, StateFailed:
		default:
			for _, path := range other.Paths {
				shared[path] = true
			}
		}
	}
	dirs := map[string]bool{}
	for _, path := range w.Paths {
		if shared[path] {
			continue
		}
		if err := os.Remove(path); err == nil {
			dirs[filepath.Dir(path)] = true
		} else if !os.IsNotExist(err) {
			// a purge that cannot delete is how a tmpfs tree fills up
			// unnoticed; it must be loud
			fmt.Fprintf(os.Stderr, "fetchd: purge %s: %v\n", path, err)
		}
	}
	for dir := range dirs {
		for dir != o.cfg.DownloadsDir && strings.HasPrefix(dir, o.cfg.DownloadsDir) {
			if err := os.Remove(dir); err != nil {
				if !os.IsNotExist(err) {
					fmt.Fprintf(os.Stderr, "fetchd: purge dir %s: %v\n", dir, err)
				}
				break
			}
			dir = filepath.Dir(dir)
		}
	}
	dropStaging(w)
	w.Paths = nil
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

// pickAssignments is the outcome of source picking: the files to
// download, which peer serves each, and each file's track index.
type pickAssignments struct {
	plan   []slskd.File
	owners map[string]string
	slots  map[string]int
}

// maxPoolLanes caps how many peers share one want: more lanes than
// this buy little and lean hard on the network's goodwill.
// minPoolShare rejects pools where a lane would carry fewer than two
// tracks on average: pooling single songs leans on peers for nothing.
const (
	maxPoolLanes = 4
	minPoolShare = 2
)

// offer is one peer's response, scored for source picking.
type offer struct {
	peer     string
	score    float64
	coverage float64
	files    []slskd.File
	flac     []slskd.File
}

// pickSource chooses the peer whose files best cover the release track
// list (falling back to count heuristics when titles are unknown) and
// returns only the files that plausibly belong to the release. When a
// set of all-flac peers covers the whole album, the files are split
// across them (up to maxPoolLanes) for parallel downloads; lossy
// formats never pool, since mp3 bitrates vary wildly per source while
// flac is flac.
func pickSource(search *slskd.Search, titles []string, trackCount int, excluded []string) (pickAssignments, error) {
	skip := map[string]bool{}
	for _, peer := range excluded {
		skip[peer] = true
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
				delta = 0 - delta
			}
			choice.coverage = float64(len(files)) / float64(trackCount)
			choice.score = 1.0 - float64(delta)/float64(trackCount)
		} else {
			choice.coverage = 1
			choice.score = 0.5
		}
		for _, file := range choice.files {
			if strings.EqualFold(filepath.Ext(remoteName(file.Filename)), ".flac") {
				choice.flac = append(choice.flac, file)
			}
		}
		if len(choice.files) > 0 {
			choice.score += 0.2 * float64(len(choice.flac)) / float64(len(choice.files))
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
		return pickAssignments{}, errors.New("no usable audio files in any response")
	}
	sort.Slice(offers, func(i, j int) bool {
		if offers[i].score != offers[j].score {
			return offers[i].score > offers[j].score
		}
		return offers[i].peer < offers[j].peer
	})

	if len(titles) > 0 {
		// pooling is the point when flac peers are plentiful: peer
		// upload caps the download, not our link, so splitting the
		// album across lanes moves more bytes per second than any
		// single host could. A complete host joins the pool like
		// everyone else and simply ends up carrying a full lane.
		if plan, err := assembleFlacPool(offers, titles); err == nil {
			return plan, nil
		}
	}

	best := offers[0]
	if len(titles) > 0 && best.coverage < minCoverage {
		return pickAssignments{}, fmt.Errorf("best peer %s covers only %d%% of the %d release tracks; nothing downloaded", best.peer, int(best.coverage*100), len(titles))
	}
	sort.Slice(best.files, func(i, j int) bool {
		return remoteName(best.files[i].Filename) < remoteName(best.files[j].Filename)
	})
	owners := map[string]string{}
	slots := map[string]int{}
	for _, file := range best.files {
		owners[file.Filename] = best.peer
		slots[file.Filename] = fileIndex(file, titles)
	}
	return pickAssignments{plan: best.files, owners: owners, slots: slots}, nil
}

// assembleFlacPool splits the album across all-flac sources when no
// single host covers it and their union does: offers are taken
// biggest-flac-first so lanes fill with large contributors, capped at
// maxPoolLanes. Any gap, or a pool whose lanes would average fewer
// than minPoolShare tracks, falls back to single-host picking.
func assembleFlacPool(offers []offer, titles []string) (pickAssignments, error) {
	if len(offers) < 2 {
		return pickAssignments{}, errors.New("pool needs multiple peers")
	}
	byFlac := make([]offer, len(offers))
	copy(byFlac, offers)
	sort.SliceStable(byFlac, func(i, j int) bool {
		return len(byFlac[i].flac) > len(byFlac[j].flac)
	})
	covered := map[int]bool{}
	plan := []slskd.File{}
	owners := map[string]string{}
	slots := map[string]int{}
	// lane count up front: capped by maxPoolLanes and by the min share
	// (a lane poolable at ~minPoolShare tracks each). Lanes are the
	// biggest flac contributors; each track goes to the least-loaded
	// feasible lane, so a complete host carries one lane's worth like
	// everyone else.
	lanes := len(titles) / minPoolShare
	if lanes > maxPoolLanes {
		lanes = maxPoolLanes
	}
	if lanes > len(byFlac) {
		lanes = len(byFlac)
	}
	if lanes < 2 {
		return pickAssignments{}, errors.New("pool too small for a split")
	}
	byFlac = byFlac[:lanes]
	claims := map[string]int{}
	for idx := range titles {
		pick := -1
		for i, offer := range byFlac {
			feasible := false
			for _, file := range offer.flac {
				if fileIndex(file, titles) == idx {
					feasible = true
					break
				}
			}
			if !feasible {
				continue
			}
			if pick < 0 || claims[offer.peer] < claims[byFlac[pick].peer] {
				pick = i
			}
		}
		if pick < 0 {
			return pickAssignments{}, fmt.Errorf("no flac peer covers track %d", idx+1)
		}
		for _, file := range byFlac[pick].flac {
			if fileIndex(file, titles) == idx {
				covered[idx] = true
				plan = append(plan, file)
				owners[file.Filename] = byFlac[pick].peer
				slots[file.Filename] = idx
				claims[byFlac[pick].peer]++
				break
			}
		}
	}
	if len(plan) < len(titles) {
		return pickAssignments{}, fmt.Errorf("flac pool covers %d of %d tracks", len(plan), len(titles))
	}
	lanesUsed := map[string]bool{}
	for _, peer := range owners {
		lanesUsed[peer] = true
	}
	if len(lanesUsed) < 2 {
		return pickAssignments{}, fmt.Errorf("flac pool collapsed to %d lane", len(lanesUsed))
	}
	sort.Slice(plan, func(i, j int) bool {
		return remoteName(plan[i].Filename) < remoteName(plan[j].Filename)
	})
	return pickAssignments{plan: plan, owners: owners, slots: slots}, nil
}

// fileIndex locates the track a soulseek file matches in the release
// list, reusing the title matcher; -1 when nothing fits.
func fileIndex(file slskd.File, titles []string) int {
	guess := strings.ToLower(fileTitleGuess(file.Filename))
	best, bestSim := -1, 0.0
	for i, title := range titles {
		if sim := match.Similarity(guess, strings.ToLower(title)); sim > bestSim {
			best, bestSim = i, sim
		}
	}
	if best < 0 || bestSim < 0.5 {
		return -1
	}
	return best
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
		o.purgeDownloads(want)
	case "needs_decision":
		if result.Decision == nil {
			return errors.New("katyd returned needs_decision without a token")
		}
		// the staged files must survive until the decision resolves
		want.State = StateNeedsPick
		want.DecisionToken = result.Decision.Token
	case "skipped":
		want.State = StateSkipped
		o.purgeDownloads(want)
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
