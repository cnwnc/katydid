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
	"strings"
	"sync"
	"time"

	"doppel.moe/katydid/internal/library"
	"doppel.moe/katydid/internal/match"
	"doppel.moe/katydid/internal/mb"
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
	albumID = filepath.Join(sanitize(albumArtist), fmt.Sprintf("%d - %s", year, sanitize(albumTitle)))
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

	stage := filepath.Join(root, ".staging", fmt.Sprintf("%d-%s", time.Now().UnixNano(), sanitize(albumTitle)))
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
		})
	}
	if unmatched > 0 {
		notes = append(notes, fmt.Sprintf("%d of %d tracks unmatched against the release, imported with file metadata", unmatched, len(ordered)))
	}
	return out, notes
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

func sanitize(name string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case '/', 0:
			return '-'
		}
		return r
	}, name)
	return strings.TrimSpace(cleaned)
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
