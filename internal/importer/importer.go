package importer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	taglib "go.senan.xyz/taglib"

	"doppel.moe/katydid/internal/library"
	"doppel.moe/katydid/internal/match"
	"doppel.moe/katydid/internal/mb"
	"doppel.moe/katydid/internal/policy"
	"doppel.moe/katydid/internal/safe"
	"doppel.moe/katydid/internal/sidecar"
	"doppel.moe/katydid/internal/tags"
)

var ErrTargetExists = errors.New("target album directory already exists")
var ErrNoDecision = errors.New("no pending decision with that token")

const (
	statusImported      = "imported"
	statusNeedsDecision = "needs_decision"
	statusSkipped       = "skipped"
)

type Request struct {
	Dir     string `json:"dir"`
	Artist  string `json:"artist,omitempty"`
	Album   string `json:"album,omitempty"`
	Year    int    `json:"year,omitempty"`
	Replace bool   `json:"replace,omitempty"`
	By      string `json:"by,omitempty"`
	Request string `json:"request,omitempty"`
}

type Decision struct {
	Token      string            `json:"token"`
	Dir        string            `json:"dir"`
	Evidence   match.Evidence    `json:"evidence"`
	Candidates []match.Candidate `json:"candidates"`
}

type Result struct {
	Status   string    `json:"status"`
	AlbumID  string    `json:"album_id,omitempty"`
	Decision *Decision `json:"decision,omitempty"`
	Notes    []string  `json:"notes,omitempty"`
}

type sourceFile struct {
	path string
	base string
	tags tags.Tags
}

type pending struct {
	request    Request
	evidence   match.Evidence
	files      []sourceFile
	candidates []match.Candidate
}

type Manager struct {
	index *library.Index
	mb    *mb.Client

	mu        sync.Mutex
	decisions map[string]*pending
}

func New(index *library.Index, client *mb.Client) *Manager {
	return &Manager{index: index, mb: client, decisions: map[string]*pending{}}
}

func (m *Manager) Import(ctx context.Context, req Request) (*Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	files, err := probeDir(req.Dir)
	if err != nil {
		return nil, err
	}
	evidence := buildEvidence(files, req)

	releases, err := m.mb.SearchReleases(ctx, mb.BuildQuery(evidence.Artist, evidence.Album))
	if err != nil {
		return nil, err
	}
	ranked := match.Rank(evidence, releases)
	if len(ranked) == 0 {
		return nil, fmt.Errorf("no musicbrainz candidates for %s - %s", evidence.Artist, evidence.Album)
	}

	if ranked[0].Auto(evidence) {
		release, err := m.mb.LookupRelease(ctx, ranked[0].ReleaseID)
		if err != nil {
			return nil, err
		}
		if coverage := match.Coverage(evidence.TrackTitles, release); coverage >= 0.8 {
			albumID, notes, err := m.publish(req, release, files)
			if err != nil {
				return nil, err
			}
			return &Result{Status: statusImported, AlbumID: albumID, Notes: notes}, nil
		}
	}

	const maxCandidates = 5
	top := ranked
	if len(top) > maxCandidates {
		top = top[:maxCandidates]
	}
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	p := &pending{request: req, evidence: evidence, files: files, candidates: top}
	m.decisions[token] = p
	return &Result{Status: statusNeedsDecision, Decision: &Decision{
		Token:      token,
		Dir:        req.Dir,
		Evidence:   evidence,
		Candidates: top,
	}}, nil
}

func (m *Manager) Decide(ctx context.Context, token string, pick int, skip bool) (*Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	p, ok := m.decisions[token]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoDecision, token)
	}
	if !skip && (pick < 1 || pick > len(p.candidates)) {
		return nil, fmt.Errorf("pick %d out of range 1..%d", pick, len(p.candidates))
	}
	delete(m.decisions, token)

	if skip {
		return &Result{Status: statusSkipped}, nil
	}

	release, err := m.mb.LookupRelease(ctx, p.candidates[pick-1].ReleaseID)
	if err != nil {
		return nil, err
	}
	albumID, notes, err := m.publish(p.request, release, p.files)
	if err != nil {
		return nil, err
	}
	return &Result{Status: statusImported, AlbumID: albumID, Notes: notes}, nil
}

func (m *Manager) Decisions() []Decision {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Decision{}
	for token, p := range m.decisions {
		out = append(out, Decision{Token: token, Dir: p.request.Dir, Evidence: p.evidence, Candidates: p.candidates})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Token < out[j].Token })
	return out
}

func probeDir(dir string) ([]sourceFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}
	files := []sourceFile{}
	for _, entry := range entries {
		if entry.IsDir() || !library.IsAudio(entry.Name()) {
			continue
		}
		base := entry.Name()
		fileTags, err := tags.Read(filepath.Join(dir, base))
		if err != nil {
			return nil, fmt.Errorf("read tags %s: %w", filepath.Join(dir, base), err)
		}
		files = append(files, sourceFile{path: filepath.Join(dir, base), base: base, tags: fileTags})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no audio files in %s", dir)
	}
	return files, nil
}

func buildEvidence(files []sourceFile, req Request) match.Evidence {
	evidence := match.Evidence{
		Artist: modalValue(files, func(t tags.Tags) string { return firstString(t.AlbumArtists, t.Artists) }),
		Album:  modalValue(files, func(t tags.Tags) string { return t.Album }),
		Year:   modalYear(files),
	}
	if req.Artist != "" {
		evidence.Artist = req.Artist
	}
	if req.Album != "" {
		evidence.Album = req.Album
	}
	if req.Year != 0 {
		evidence.Year = req.Year
	}
	evidence.TrackCount = len(files)
	evidence.TrackTitles = make([]string, 0, len(files))
	for _, file := range sortFiles(files) {
		evidence.TrackTitles = append(evidence.TrackTitles, file.tags.Title)
	}
	return evidence
}

func firstString(values []string, fallback []string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	for _, v := range fallback {
		if v != "" {
			return v
		}
	}
	return ""
}

func modalValue(files []sourceFile, pick func(tags.Tags) string) string {
	counts := map[string]int{}
	for _, file := range files {
		if value := strings.TrimSpace(pick(file.tags)); value != "" {
			counts[value]++
		}
	}
	best, bestCount := "", 0
	for value, count := range counts {
		if count > bestCount || (count == bestCount && value < best) {
			best, bestCount = value, count
		}
	}
	return best
}

func modalYear(files []sourceFile) int {
	counts := map[int]int{}
	for _, file := range files {
		if file.tags.Year != 0 {
			counts[file.tags.Year]++
		}
	}
	best, bestCount := 0, 0
	for year, count := range counts {
		if count > bestCount || (count == bestCount && year > best) {
			best, bestCount = year, count
		}
	}
	return best
}

func sortFiles(files []sourceFile) []sourceFile {
	out := make([]sourceFile, len(files))
	copy(out, files)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].tags, out[j].tags
		if a.DiscNumber != b.DiscNumber {
			return a.DiscNumber < b.DiscNumber
		}
		if a.TrackNumber != b.TrackNumber {
			return a.TrackNumber < b.TrackNumber
		}
		return out[i].base < out[j].base
	})
	return out
}

func (m *Manager) publish(req Request, release *mb.Release, files []sourceFile) (albumID string, notes []string, err error) {
	root := m.index.Root()
	cfg, err := policy.Ensure(root)
	if err != nil {
		return "", nil, err
	}
	pol, err := cfg.Policy("")
	if err != nil {
		return "", nil, err
	}

	albumArtist := release.Artist()
	if albumArtist == "" {
		albumArtist = firstNonEmpty(req.Artist, "Unknown Artist")
	}
	year := match.YearOf(release.Date)
	if year == 0 {
		year = modalYear(files)
		notes = append(notes, fmt.Sprintf("release has no date, used file year %d", year))
	}
	albumTitle := release.Title

	dir, err := policy.PlanDir(pol, albumArtist, albumTitle, year)
	if err != nil {
		return "", nil, err
	}
	albumID = dir
	target := filepath.Join(root, albumID)

	if _, err := os.Stat(target); err == nil {
		if !req.Replace {
			return "", nil, fmt.Errorf("%w: %s (use replace to overwrite)", ErrTargetExists, albumID)
		}
		if err := trash(root, target); err != nil {
			return "", nil, err
		}
		notes = append(notes, "previous album moved to .trash")
	}

	stage := filepath.Join(root, ".staging", fmt.Sprintf("%d-%s", time.Now().UnixNano(), safe.Name(albumTitle, "album")))
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return "", nil, fmt.Errorf("create staging dir: %w", err)
	}
	defer os.RemoveAll(stage)

	copied, err := copyFiles(files, stage)
	if err != nil {
		return "", nil, err
	}

	tracks, trackNotes := mapTracks(copied, release)
	notes = append(notes, trackNotes...)

	sc := &sidecar.Album{
		Album:       albumTitle,
		AlbumArtist: albumArtist,
		Year:        year,
		Provenance: sidecar.Provenance{
			Imported: time.Now().UTC(),
			By:       firstNonEmpty(req.By, "cli"),
			Request:  req.Request,
			Origin:   sidecar.Origin{Path: req.Dir},
		},
		Tracks: tracks,
	}
	sc.MusicBrainz.ReleaseID = release.ID
	if release.ReleaseGroup != nil {
		sc.MusicBrainz.ReleaseGroupID = release.ReleaseGroup.ID
	}
	sc.MusicBrainz.Date = release.Date
	sc.Label = firstLabel(release)
	sc.CatalogNumber = firstCatalogNumber(release)

	renamed, err := applyTagsAndNames(stage, sc, pol)
	if err != nil {
		return "", nil, err
	}
	notes = append(notes, renamed...)

	if err := sidecar.Save(stage, sc); err != nil {
		return "", nil, err
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", nil, err
	}
	if err := os.Rename(stage, target); err != nil {
		return "", nil, fmt.Errorf("publish %s: %w", target, err)
	}
	if err := m.index.Scan(); err != nil {
		return "", nil, fmt.Errorf("rescan after import: %w", err)
	}
	return albumID, notes, nil
}

func firstLabel(release *mb.Release) string {
	for _, info := range release.LabelInfo {
		if info.Label != nil && info.Label.Name != "" {
			return info.Label.Name
		}
	}
	return ""
}

func firstCatalogNumber(release *mb.Release) string {
	for _, info := range release.LabelInfo {
		if info.CatalogNumber != "" {
			return info.CatalogNumber
		}
	}
	return ""
}

// applyTagsAndNames renames staged files to the policy template, writes the
// policy payload into each file's tags, and records both in the sidecar.
func applyTagsAndNames(stage string, sc *sidecar.Album, pol policy.Policy) ([]string, error) {
	notes := []string{}
	discTotal := 0
	for _, track := range sc.Tracks {
		if track.Disc > discTotal {
			discTotal = track.Disc
		}
	}

	used := map[string]bool{}
	files := make([]string, 0, len(sc.Tracks))
	payloads := make([]map[string][]string, 0, len(sc.Tracks))

	for i := range sc.Tracks {
		track := &sc.Tracks[i]
		ext := filepath.Ext(track.File)
		payload := payloadForTrack(sc, track, pol, len(sc.Tracks), discTotal)

		planned, err := policy.PlanFilename(pol, track.Disc, track.Track, track.Title, ext, discTotal)
		if err != nil {
			return nil, fmt.Errorf("plan filename for %s: %w", track.File, err)
		}
		if used[planned] || planned == track.File {
			planned = track.File
		}
		if used[planned] {
			notes = append(notes, fmt.Sprintf("kept original name %s (planned name taken)", track.File))
		} else if planned != track.File {
			if err := os.Rename(filepath.Join(stage, track.File), filepath.Join(stage, planned)); err != nil {
				return nil, fmt.Errorf("rename %s to %s: %w", track.File, planned, err)
			}
			track.File = planned
		}
		used[planned] = true

		path := filepath.Join(stage, track.File)
		if err := taglib.WriteTags(path, payload, taglib.Clear); err != nil {
			return nil, fmt.Errorf("write tags %s: %w", path, err)
		}
		track.Tags = payload
		files = append(files, track.File)
		payloads = append(payloads, payload)
	}

	sc.TagState = &sidecar.TagState{
		Policy:    pol.Name,
		Applied:   time.Now().UTC(),
		StateHash: policy.HashTracks(files, payloads),
	}
	return notes, nil
}

// payloadForTrack builds the full raw tag map for one track from the album
// metadata, then runs it through the policy. The result is the exact payload
// written into the file.
func payloadForTrack(sc *sidecar.Album, track *sidecar.Track, pol policy.Policy, trackTotal, discTotal int) map[string][]string {
	artist := []string{sc.AlbumArtist}
	if len(track.Artists) > 0 {
		artist = track.Artists
	}
	raw := map[string][]string{
		"TITLE":       {track.Title},
		"ARTIST":      artist,
		"ALBUMARTIST": {sc.AlbumArtist},
		"ALBUM":       {sc.Album},
		"TRACKNUMBER": {fmt.Sprintf("%d/%d", track.Track, trackTotal)},
		"DATE":        {firstNonEmpty(sc.MusicBrainz.Date, strconv.Itoa(sc.Year))},
	}
	if discTotal > 1 {
		raw["DISCNUMBER"] = []string{fmt.Sprintf("%d/%d", track.Disc, discTotal)}
	}
	if sc.MusicBrainz.ReleaseID != "" {
		raw["MUSICBRAINZ_ALBUMID"] = []string{sc.MusicBrainz.ReleaseID}
	}
	if sc.MusicBrainz.ReleaseGroupID != "" {
		raw["MUSICBRAINZ_RELEASEGROUPID"] = []string{sc.MusicBrainz.ReleaseGroupID}
	}
	if track.RecordingID != "" {
		raw["MUSICBRAINZ_TRACKID"] = []string{track.RecordingID}
	}
	if sc.Label != "" {
		raw["LABEL"] = []string{sc.Label}
	}
	if sc.CatalogNumber != "" {
		raw["CATALOGNUMBER"] = []string{sc.CatalogNumber}
	}
	return policy.Apply(pol, raw)
}

type stagedFile struct {
	base string
	tags tags.Tags
}

func copyFiles(files []sourceFile, stage string) ([]stagedFile, error) {
	out := make([]stagedFile, 0, len(files))
	for _, file := range files {
		dest := filepath.Join(stage, file.base)
		if err := copyFile(file.path, dest); err != nil {
			return nil, err
		}
		out = append(out, stagedFile{base: file.base, tags: file.tags})
	}
	return out, nil
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy %s to %s: %w", src, dest, err)
	}
	return nil
}

func mapTracks(files []stagedFile, release *mb.Release) ([]sidecar.Track, []string) {
	tracks := release.FlattenedTracks()
	notes := []string{}

	ordered := make([]stagedFile, len(files))
	copy(ordered, files)
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i].tags, ordered[j].tags
		if a.DiscNumber != b.DiscNumber {
			return a.DiscNumber < b.DiscNumber
		}
		if a.TrackNumber != b.TrackNumber {
			return a.TrackNumber < b.TrackNumber
		}
		return ordered[i].base < ordered[j].base
	})

	if len(ordered) == len(tracks) {
		out := make([]sidecar.Track, 0, len(ordered))
		mediumOf := mediumPositions(release)
		for i, file := range ordered {
			out = append(out, sidecar.Track{
				File:          file.base,
				Title:         tracks[i].Title,
				Track:         tracks[i].Position,
				Disc:          mediumOf[i],
				LengthSeconds: trackLength(tracks[i], file),
				RecordingID:   tracks[i].ID,
				Artists:       trackArtists(tracks[i], file),
			})
		}
		return out, notes
	}

	used := make([]bool, len(tracks))
	out := make([]sidecar.Track, 0, len(ordered))
	mediumOf := mediumPositions(release)
	unmatched := 0
	for _, file := range ordered {
		index := -1

		if file.tags.DiscNumber != 0 && file.tags.TrackNumber != 0 {
			for i, track := range tracks {
				if !used[i] && mediumOf[i] == file.tags.DiscNumber && track.Position == file.tags.TrackNumber {
					index = i
					break
				}
			}
		}
		if index < 0 {
			best, bestSim := -1, 0.0
			for i, track := range tracks {
				if used[i] {
					continue
				}
				if sim := match.Similarity(file.tags.Title, track.Title); sim > bestSim {
					best, bestSim = i, sim
				}
			}
			if bestSim >= 0.6 {
				index = best
			}
		}
		if index < 0 {
			unmatched++
			out = append(out, sidecar.Track{
				File:          file.base,
				Title:         firstNonEmpty(file.tags.Title, strings.TrimSuffix(file.base, filepath.Ext(file.base))),
				Track:         file.tags.TrackNumber,
				Disc:          file.tags.DiscNumber,
				LengthSeconds: file.tags.LengthSeconds,
				Artists:       trackArtistsFromTags(file),
			})
			continue
		}
		used[index] = true
		out = append(out, sidecar.Track{
			File:          file.base,
			Title:         tracks[index].Title,
			Track:         tracks[index].Position,
			Disc:          mediumOf[index],
			LengthSeconds: trackLength(tracks[index], file),
			RecordingID:   tracks[index].ID,
			Artists:       trackArtists(tracks[index], file),
		})
	}
	if unmatched > 0 {
		notes = append(notes, fmt.Sprintf("%d of %d tracks unmatched against the release, imported with file metadata", unmatched, len(ordered)))
	}
	return out, notes
}

// RetagResult reports one album's retag outcome.
type RetagResult struct {
	AlbumID string   `json:"album_id"`
	Notes   []string `json:"notes,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// Retag brings one sidecar-backed album under a policy: renames files to the
// template, rewrites tags to the payload, and records the new tag state.
func (m *Manager) Retag(albumID, policyName string) RetagResult {
	root := m.index.Root()
	result := RetagResult{AlbumID: albumID}

	cfg, err := policy.Ensure(root)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	pol, err := cfg.Policy(policyName)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	dir := filepath.Join(root, albumID)
	sc, err := sidecar.Load(dir)
	if err != nil {
		result.Error = fmt.Sprintf("load sidecar: %v", err)
		return result
	}

	stage := filepath.Join(root, ".staging", fmt.Sprintf("retag-%d-%s", time.Now().UnixNano(), safe.Name(sc.Album, "album")))
	if err := os.MkdirAll(stage, 0o755); err != nil {
		result.Error = fmt.Sprintf("create staging dir: %v", err)
		return result
	}
	defer os.RemoveAll(stage)

	for _, track := range sc.Tracks {
		if err := copyFile(filepath.Join(dir, track.File), filepath.Join(stage, track.File)); err != nil {
			result.Error = err.Error()
			return result
		}
	}
	for _, entry := range []string{"cover.jpg", "cover.png"} {
		if _, err := os.Stat(filepath.Join(dir, entry)); err == nil {
			if err := copyFile(filepath.Join(dir, entry), filepath.Join(stage, entry)); err != nil {
				result.Error = err.Error()
				return result
			}
		}
	}

	discTotal := 0
	for _, track := range sc.Tracks {
		if track.Disc > discTotal {
			discTotal = track.Disc
		}
	}

	notes := []string{}
	used := map[string]bool{}
	files := make([]string, 0, len(sc.Tracks))
	payloads := make([]map[string][]string, 0, len(sc.Tracks))
	for i := range sc.Tracks {
		track := &sc.Tracks[i]
		payload := track.Tags
		if payload == nil {
			payload = payloadForTrack(sc, track, pol, len(sc.Tracks), discTotal)
			notes = append(notes, fmt.Sprintf("rebuilt payload for %s from sidecar fields", track.File))
		}

		planned, err := policy.PlanFilename(pol, track.Disc, track.Track, track.Title, filepath.Ext(track.File), discTotal)
		if err != nil {
			result.Error = fmt.Sprintf("plan filename for %s: %v", track.File, err)
			return result
		}
		if used[planned] || planned == track.File {
			planned = track.File
		}
		if used[planned] {
			notes = append(notes, fmt.Sprintf("kept original name %s (planned name taken)", track.File))
		} else if planned != track.File {
			if err := os.Rename(filepath.Join(stage, track.File), filepath.Join(stage, planned)); err != nil {
				result.Error = fmt.Sprintf("rename %s: %v", track.File, err)
				return result
			}
			track.File = planned
		}
		used[planned] = true

		path := filepath.Join(stage, track.File)
		if err := taglib.WriteTags(path, payload, taglib.Clear); err != nil {
			result.Error = fmt.Sprintf("write tags %s: %v", track.File, err)
			return result
		}
		track.Tags = payload
		files = append(files, track.File)
		payloads = append(payloads, payload)
	}

	sc.TagState = &sidecar.TagState{
		Policy:    pol.Name,
		Applied:   time.Now().UTC(),
		StateHash: policy.HashTracks(files, payloads),
	}
	if err := sidecar.Save(stage, sc); err != nil {
		result.Error = fmt.Sprintf("save sidecar: %v", err)
		return result
	}

	if err := trash(root, dir); err != nil {
		result.Error = fmt.Sprintf("trash old album: %v", err)
		return result
	}
	if err := os.Rename(stage, dir); err != nil {
		result.Error = fmt.Sprintf("publish %s: %v", dir, err)
		return result
	}
	if err := m.index.Scan(); err != nil {
		result.Error = fmt.Sprintf("rescan: %v", err)
		return result
	}
	result.Notes = notes
	return result
}

// RetagAll retags every sidecar-backed album and skips pending ones.
func (m *Manager) RetagAll(policyName string) []RetagResult {
	results := []RetagResult{}
	for _, album := range m.index.Albums(library.Query{}) {
		if album.Pending {
			results = append(results, RetagResult{AlbumID: album.ID, Error: "pending import, skipped"})
			continue
		}
		results = append(results, m.Retag(album.ID, policyName))
	}
	return results
}

func trackArtists(track mb.ReleaseTrack, file stagedFile) []string {
	if credit := mb.CreditName(track.ArtistCredit); credit != "" {
		return []string{credit}
	}
	return trackArtistsFromTags(file)
}

func trackArtistsFromTags(file stagedFile) []string {
	if len(file.tags.Artists) > 0 {
		return file.tags.Artists
	}
	if len(file.tags.AlbumArtists) > 0 {
		return file.tags.AlbumArtists
	}
	return nil
}

func mediumPositions(release *mb.Release) []int {
	positions := []int{}
	media := make([]mb.ReleaseMedia, len(release.Media))
	copy(media, release.Media)
	sort.Slice(media, func(i, j int) bool { return media[i].Position < media[j].Position })
	for _, medium := range media {
		for i := 0; i < medium.TrackCount && i < len(medium.Tracks); i++ {
			positions = append(positions, medium.Position)
		}
	}
	return positions
}

func trackLength(track mb.ReleaseTrack, file stagedFile) float64 {
	if track.Length > 0 {
		return float64(track.Length) / 1000
	}
	return file.tags.LengthSeconds
}

func trash(root, target string) error {
	trashDir := filepath.Join(root, ".trash")
	if err := os.MkdirAll(trashDir, 0o755); err != nil {
		return fmt.Errorf("create trash dir: %w", err)
	}
	dest := filepath.Join(trashDir, fmt.Sprintf("%d-%s", time.Now().UnixNano(), filepath.Base(target)))
	if err := os.Rename(target, dest); err != nil {
		return fmt.Errorf("move %s to trash: %w", target, err)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func newToken() (string, error) {
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}
