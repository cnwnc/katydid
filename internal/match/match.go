package match

import (
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"doppel.moe/katydid/internal/mb"
)

const (
	AutoThreshold      = 0.90
	AutoTitleSim       = 0.75
	AutoTrackCountSim  = 0.80
	SpecifierTitleSim  = 0.90
	SpecifierArtistSim = 0.90
	minSearchScore     = 40
	maxAutoTrackTitles = 50
)

// SpecifierAuto gates an auto-pick made from a bare specifier alone (no file
// evidence): names must be near-exact, and a requested year must agree. This
// gates what to search soulseek for, not what to import; the import gate
// still runs later with real files.
func SpecifierAuto(c Candidate, year int) bool {
	if c.TitleSim < SpecifierTitleSim || c.ArtistSim < SpecifierArtistSim {
		return false
	}
	if year != 0 && c.YearSim < 0.4 {
		return false
	}
	return true
}

type Evidence struct {
	Artist      string   `json:"artist"`
	Album       string   `json:"album"`
	Year        int      `json:"year"`
	TrackCount  int      `json:"track_count"`
	TrackTitles []string `json:"track_titles"`
}

type Candidate struct {
	Number        int     `json:"number"`
	ReleaseID     string  `json:"release_id"`
	GroupID       string  `json:"group_id,omitempty"`
	Title         string  `json:"title"`
	Artist        string  `json:"artist"`
	Date          string  `json:"date,omitempty"`
	TrackCount    int     `json:"track_count"`
	Score         float64 `json:"score"`
	TitleSim      float64 `json:"title_sim"`
	ArtistSim     float64 `json:"artist_sim"`
	TrackCountSim float64 `json:"track_count_sim"`
	YearSim       float64 `json:"year_sim"`
	MBSearchScore int     `json:"mb_search_score"`
}

func (c Candidate) Auto(ev Evidence) bool {
	return c.Score >= AutoThreshold &&
		c.TitleSim >= AutoTitleSim &&
		c.TrackCountSim >= AutoTrackCountSim &&
		c.YearSim >= 0.4
}

const (
	weightTitle      = 0.35
	weightArtist     = 0.25
	weightTrackCount = 0.25
	weightYear       = 0.15
)

func Rank(ev Evidence, releases []mb.SearchRelease) []Candidate {
	byGroup := map[string]Candidate{}
	ungrouped := []Candidate{}

	for _, release := range releases {
		if release.Score < minSearchScore {
			continue
		}
		candidate := scoreCandidate(ev, release)
		groupID := ""
		if release.ReleaseGroup != nil {
			groupID = release.ReleaseGroup.ID
		}
		if groupID == "" {
			ungrouped = append(ungrouped, candidate)
			continue
		}
		if existing, ok := byGroup[groupID]; !ok || candidate.Score > existing.Score {
			byGroup[groupID] = candidate
		}
	}

	all := append(ungrouped, mapValues(byGroup)...)
	sort.Slice(all, func(i, j int) bool {
		if all[i].Score != all[j].Score {
			return all[i].Score > all[j].Score
		}
		return all[i].MBSearchScore > all[j].MBSearchScore
	})
	for i := range all {
		all[i].Number = i + 1
	}
	return all
}

func scoreCandidate(ev Evidence, release mb.SearchRelease) Candidate {
	titleSim := Similarity(ev.Album, release.Title)
	artistSim := Similarity(ev.Artist, release.Artist())
	trackCountSim := trackCountSimilarity(ev.TrackCount, release.TrackCount)
	yearSim := yearSimilarity(ev.Year, yearOf(release.Date))

	candidate := Candidate{
		ReleaseID:     release.ID,
		Title:         release.Title,
		Artist:        release.Artist(),
		Date:          release.Date,
		TrackCount:    release.TrackCount,
		TitleSim:      titleSim,
		ArtistSim:     artistSim,
		TrackCountSim: trackCountSim,
		YearSim:       yearSim,
		MBSearchScore: release.Score,
	}
	if release.ReleaseGroup != nil {
		candidate.GroupID = release.ReleaseGroup.ID
	}
	candidate.Score = weightTitle*titleSim +
		weightArtist*artistSim +
		weightTrackCount*trackCountSim +
		weightYear*yearSim
	return candidate
}

func trackCountSimilarity(files, release int) float64 {
	if files == 0 || release == 0 {
		return 0.5
	}
	big, small := files, release
	if small > big {
		big, small = small, big
	}
	return float64(small) / float64(big)
}

func yearSimilarity(a, b int) float64 {
	switch {
	case a == 0 && b == 0:
		return 0.5
	case a == 0:
		return 0.5
	case b == 0:
		return 0.3
	}
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	switch {
	case diff <= 1:
		return 1
	case diff <= 5:
		return 0.8
	case diff <= 10:
		return 0.5
	default:
		return 0.2
	}
}

func yearOf(date string) int {
	return YearOf(date)
}

func YearOf(date string) int {
	if len(date) < 4 {
		return 0
	}
	year := 0
	for _, ch := range date[:4] {
		if ch < '0' || ch > '9' {
			return 0
		}
		year = year*10 + int(ch-'0')
	}
	return year
}

const coverageThreshold = 0.8

func Coverage(titles []string, release *mb.Release) float64 {
	if len(titles) == 0 || release == nil {
		return 0
	}
	trackTitles := []string{}
	for _, track := range release.FlattenedTracks() {
		trackTitles = append(trackTitles, track.Title)
	}
	checked := titles
	if len(checked) > maxAutoTrackTitles {
		checked = checked[:maxAutoTrackTitles]
	}
	total := 0.0
	for _, title := range checked {
		best := 0.0
		for _, trackTitle := range trackTitles {
			if sim := Similarity(title, trackTitle); sim > best {
				best = sim
			}
		}
		total += best
	}
	return total / float64(len(checked))
}

func Similarity(a, b string) float64 {
	a, b = normalize(a), normalize(b)
	if a == b {
		return 1
	}
	if a == "" || b == "" {
		return 0
	}
	if len(a) < 3 || len(b) < 3 {
		if strings.Contains(a, b) || strings.Contains(b, a) {
			return 0.7
		}
		return 0
	}
	return dice(a, b)
}

func dice(a, b string) float64 {
	setA, setB := trigrams(a), trigrams(b)
	intersection := 0
	for gram := range setA {
		if _, ok := setB[gram]; ok {
			intersection++
		}
	}
	return 2 * float64(intersection) / float64(len(setA)+len(setB))
}

func trigrams(s string) map[string]struct{} {
	padded := " " + s + " "
	out := map[string]struct{}{}
	runes := []rune(padded)
	for i := 0; i+3 <= len(runes); i++ {
		out[string(runes[i:i+3])] = struct{}{}
	}
	return out
}

func normalize(s string) string {
	s = norm.NFC.String(s)
	var builder strings.Builder
	lastSpace := true
	for _, ch := range strings.ToLower(s) {
		if unicode.IsLetter(ch) || unicode.IsDigit(ch) {
			builder.WriteRune(ch)
			lastSpace = false
		} else if !lastSpace {
			builder.WriteRune(' ')
			lastSpace = true
		}
	}
	return strings.TrimSpace(builder.String())
}

func mapValues[V any](m map[string]V) []V {
	out := make([]V, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
