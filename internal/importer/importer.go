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
	ext  string
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
	evidence, err := buildEvidence(files, req)
	if err != nil {
		return nil, err
	}

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

// Resolve ranks musicbrainz candidates for a bare specifier (no files). Used
// by the fetcher to choose what to search soulseek for; the import gate runs
// later with real evidence.
func (m *Manager) Resolve(ctx context.Context, artist, album string, year int, mbid string) ([]match.Candidate, error) {
	if artist == "" || album == "" {
		return nil, errors.New("artist and album are required")
	}
	if mbid != "" {
		release, err := m.mb.LookupRelease(ctx, mbid)
		if err != nil {
			return nil, err
		}
		evidence := match.Evidence{Artist: artist, Album: album, Year: year}
		return match.Rank(evidence, []mb.SearchRelease{searchFromRelease(release)}), nil
	}
	releases, err := m.mb.SearchReleases(ctx, mb.BuildQuery(artist, album))
	if err != nil {
		return nil, err
	}
	evidence := match.Evidence{Artist: artist, Album: album, Year: year}
	ranked := match.Rank(evidence, releases)
	const maxCandidates = 5
	if len(ranked) > maxCandidates {
		ranked = ranked[:maxCandidates]
	}
	return ranked, nil
}

func searchFromRelease(release *mb.Release) mb.SearchRelease {
	search := mb.SearchRelease{
		ID:           release.ID,
		Score:        100,
		Title:        release.Title,
		Status:       release.Status,
		Date:         release.Date,
		ArtistCredit: release.ArtistCredit,
	}
	if release.ReleaseGroup != nil {
		search.ReleaseGroup = &struct {
			ID          string `json:"id"`
			Title       string `json:"title"`
			PrimaryType string `json:"primary-type"`
		}{ID: release.ReleaseGroup.ID, Title: release.Title, PrimaryType: release.ReleaseGroup.PrimaryType}
	}
	for _, medium := range release.Media {
		search.TrackCount += medium.TrackCount
	}
	if search.TrackCount == 0 {
		search.TrackCount = len(release.FlattenedTracks())
	}
	return search
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

func buildEvidence(files []sourceFile, req Request) (match.Evidence, error) {
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
	if evidence.Artist == "" && evidence.Album == "" {
		return evidence, fmt.Errorf("no artist or album tags in %s; provide -artist and -album hints", firstNonEmpty(req.Dir, "the source dir"))
	}
	evidence.TrackCount = len(files)
	evidence.TrackTitles = make([]string, 0, len(files))
	for _, file := range sortFiles(files) {
		title := file.tags.Title
		if title == "" {
			title = titleFromFilename(file.base)
		}
		evidence.TrackTitles = append(evidence.TrackTitles, title)
	}
	return evidence, nil
}

// titleFromFilename recovers a track title from "01 Notres Langues.flac".
func titleFromFilename(base string) string {
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	trimmed := strings.TrimLeft(stem, " 0123456789-")
	if trimmed == "" {
		return stem
	}
	return strings.TrimSpace(trimmed)
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

	if _, err := os.Stat(target); err == nil && !req.Replace {
		return "", nil, fmt.Errorf("%w: %s (use replace to overwrite)", ErrTargetExists, albumID)
	}
	replaced := false
	if _, err := os.Stat(target); err == nil {
		replaced = true
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
	sc.AlbumArtists = distinctOr(mb.CreditList(release.ArtistCredit), []string{albumArtist})
	sc.AlbumArtistSort = mb.SortName(release.ArtistCredit)
	sc.OriginalDate = release.OriginalDate()
	sc.ReleaseType = release.ReleaseType()
	sc.Compilation = release.IsCompilation()
	sc.Genres = release.TopGenres(1)
	if len(sc.Genres) == 0 {
		if genre := modalGenre(files); genre != "" {
			sc.Genres = []string{genre}
		}
	}

	renamed, err := applyTagsAndNames(stage, sc, pol, copied)
	if err != nil {
		return "", nil, err
	}
	notes = append(notes, renamed...)

	if err := sidecar.Save(stage, sc); err != nil {
		return "", nil, err
	}

	if replaced {
		if err := trash(root, target); err != nil {
			return "", nil, err
		}
		notes = append(notes, "previous album moved to .trash")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", nil, err
	}
	if err := os.Rename(stage, target); err != nil {
		if replaced {
			if rerr := restoreFromTrash(root, target); rerr != nil {
				return "", nil, fmt.Errorf("publish %s: %v (restore also failed: %v)", target, err, rerr)
			}
		}
		return "", nil, fmt.Errorf("publish %s: %w", target, err)
	}
	if err := m.index.Scan(); err != nil {
		return "", nil, fmt.Errorf("rescan after import: %w", err)
	}
	return albumID, notes, nil
}

// restoreFromTrash moves the most recent trash entry for a target back
// into place. It is the safety net for a failed publish-after-trash.
func restoreFromTrash(root, target string) error {
	entries, err := os.ReadDir(filepath.Join(root, ".trash"))
	if err != nil {
		return err
	}
	suffix := "-" + filepath.Base(target)
	bestName, bestTime := "", int64(-1)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), suffix) {
			continue
		}
		prefix := strings.TrimSuffix(entry.Name(), suffix)
		nanos, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil || nanos <= bestTime {
			continue
		}
		bestName, bestTime = entry.Name(), nanos
	}
	if bestName == "" {
		return fmt.Errorf("no trash entry for %s", filepath.Base(target))
	}
	return os.Rename(filepath.Join(root, ".trash", bestName), target)
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
// Track totals are per disc on multi-disc albums and album-wide otherwise.
func applyTagsAndNames(stage string, sc *sidecar.Album, pol policy.Policy, staged []stagedFile) ([]string, error) {
	notes := []string{}
	discTotal := 0
	perDisc := map[int]int{}
	for _, track := range sc.Tracks {
		if track.Disc > discTotal {
			discTotal = track.Disc
		}
		perDisc[track.Disc]++
	}
	exts := map[string]string{}
	for _, file := range staged {
		exts[file.base] = file.ext
	}

	used := map[string]bool{}
	files := make([]string, 0, len(sc.Tracks))
	payloads := make([]map[string][]string, 0, len(sc.Tracks))

	for i := range sc.Tracks {
		track := &sc.Tracks[i]
		ext := filepath.Ext(track.File)
		if ext == "" {
			ext = exts[track.File]
		}
		trackTotal := len(sc.Tracks)
		if discTotal > 1 {
			trackTotal = perDisc[track.Disc]
		}
		payload := payloadForTrack(sc, track, pol, trackTotal, discTotal)

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
	artistDisplay := firstNonEmpty(track.Artist, sc.AlbumArtist)
	artists := track.Artists
	if len(artists) == 0 {
		artists = []string{artistDisplay}
	}
	raw := map[string][]string{
		"TITLE":       {track.Title},
		"ARTIST":      {artistDisplay},
		"ARTISTS":     artists,
		"ALBUMARTIST": {sc.AlbumArtist},
		"ALBUM":       {sc.Album},
		"TRACKNUMBER": {strconv.Itoa(track.Track)},
		"TRACKTOTAL":  {strconv.Itoa(trackTotal)},
		"DATE":        {firstNonEmpty(sc.MusicBrainz.Date, strconv.Itoa(sc.Year))},
	}
	if discTotal > 1 {
		raw["DISCNUMBER"] = []string{strconv.Itoa(track.Disc)}
		raw["DISCTOTAL"] = []string{strconv.Itoa(discTotal)}
	}
	if sc.AlbumArtistSort != "" {
		raw["ALBUMARTISTSORT"] = []string{sc.AlbumArtistSort}
	}
	if track.ArtistSort != "" {
		raw["ARTISTSORT"] = []string{track.ArtistSort}
	}
	if len(sc.Genres) > 0 {
		raw["GENRE"] = sc.Genres
	}
	if sc.OriginalDate != "" {
		raw["ORIGINALDATE"] = []string{sc.OriginalDate}
		raw["ORIGINALYEAR"] = []string{sc.OriginalDate[:minInt(4, len(sc.OriginalDate))]}
	}
	if sc.ReleaseType != "" {
		raw["RELEASETYPE"] = []string{sc.ReleaseType}
	}
	if sc.Compilation {
		raw["COMPILATION"] = []string{"1"}
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
	if track.ReleaseTrackID != "" {
		raw["MUSICBRAINZ_RELEASETRACKID"] = []string{track.ReleaseTrackID}
	}
	if sc.Label != "" {
		raw["LABEL"] = []string{sc.Label}
	}
	if sc.CatalogNumber != "" {
		raw["CATALOGNUMBER"] = []string{sc.CatalogNumber}
	}
	return policy.Apply(pol, raw)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type stagedFile struct {
	base string
	ext  string
	tags tags.Tags
}

func copyFiles(files []sourceFile, stage string) ([]stagedFile, error) {
	out := make([]stagedFile, 0, len(files))
	for _, file := range files {
		dest := filepath.Join(stage, file.base)
		if err := copyFile(file.path, dest); err != nil {
			return nil, err
		}
		out = append(out, stagedFile{base: file.base, ext: file.ext, tags: file.tags})
	}
	return out, nil
}

func probeDir(dir string) ([]sourceFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}
	files := []sourceFile{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		base := entry.Name()
		ext := filepath.Ext(base)
		if !library.IsAudio(base) {
			if library.IsAudio(base + sniffedExt(filepath.Join(dir, base))) {
				ext = sniffedExt(filepath.Join(dir, base))
			} else {
				continue
			}
		}
		fileTags, err := tags.Read(filepath.Join(dir, base))
		if err != nil {
			return nil, fmt.Errorf("read tags %s: %w", filepath.Join(dir, base), err)
		}
		files = append(files, sourceFile{path: filepath.Join(dir, base), base: base, ext: ext, tags: fileTags})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no audio files in %s", dir)
	}
	return files, nil
}

// sniffedExt identifies an audio file by content when the name carries no
// usable extension. Returns "" when the bytes are unrecognized.
func sniffedExt(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()

	header := make([]byte, 12)
	read, err := file.Read(header)
	if err != nil || read < 4 {
		return ""
	}
	switch {
	case string(header[0:4]) == "fLaC":
		return ".flac"
	case string(header[0:3]) == "ID3":
		return ".mp3"
	case string(header[0:4]) == "OggS":
		return ".ogg"
	case string(header[4:8]) == "ftyp":
		return ".m4a"
	case string(header[0:4]) == "RIFF" && read >= 12 && string(header[8:12]) == "WAVE":
		return ".wav"
	}
	return ""
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
			out = append(out, trackFromRelease(file, tracks[i], mediumOf[i]))
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
				if sim := match.Similarity(firstNonEmpty(file.tags.Title, titleFromFilename(file.base)), track.Title); sim > bestSim {
					best, bestSim = i, sim
				}
			}
			if bestSim >= 0.6 {
				index = best
			}
		}
		if index < 0 {
			unmatched++
			out = append(out, trackFromFileOnly(file))
			continue
		}
		used[index] = true
		out = append(out, trackFromRelease(file, tracks[index], mediumOf[index]))
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
		payload := payloadForTrack(sc, track, pol, len(sc.Tracks), discTotal)

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
		if rerr := restoreFromTrash(root, dir); rerr != nil {
			result.Error = fmt.Sprintf("publish %s: %v (restore also failed: %v)", dir, err, rerr)
			return result
		}
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

func trackFromRelease(file stagedFile, releaseTrack mb.ReleaseTrack, disc int) sidecar.Track {
	credit := mb.CreditName(releaseTrack.ArtistCredit)
	if credit == "" {
		credit = firstNonEmpty(file.tags.Artists...)
	}
	return sidecar.Track{
		File:           file.base,
		Title:          releaseTrack.Title,
		Track:          releaseTrack.Position,
		Disc:           disc,
		LengthSeconds:  trackLength(releaseTrack, file),
		Artist:         credit,
		ArtistSort:     mb.SortName(releaseTrack.ArtistCredit),
		Artists:        firstNonEmptyList(mb.CreditList(releaseTrack.ArtistCredit), trackArtistsFromTags(file)),
		RecordingID:    releaseTrack.Recording.ID,
		ReleaseTrackID: releaseTrack.ID,
	}
}

func trackFromFileOnly(file stagedFile) sidecar.Track {
	artists := trackArtistsFromTags(file)
	title := firstNonEmpty(file.tags.Title, titleFromFilename(file.base))
	return sidecar.Track{
		File:          file.base,
		Title:         title,
		Track:         file.tags.TrackNumber,
		Disc:          file.tags.DiscNumber,
		LengthSeconds: file.tags.LengthSeconds,
		Artist:        firstNonEmpty(artists...),
		Artists:       artists,
	}
}

func firstNonEmptyList(a, b []string) []string {
	if len(a) > 0 {
		return a
	}
	return b
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

func distinctOr(a, b []string) []string {
	if len(a) > 0 {
		return a
	}
	return b
}

func modalGenre(files []sourceFile) string {
	return modalValue(files, func(t tags.Tags) string {
		for _, genre := range t.Genres {
			if strings.TrimSpace(genre) != "" {
				return strings.TrimSpace(genre)
			}
		}
		return ""
	})
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
