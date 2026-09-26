package discord

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
)

const (
	labelLimit        = 100
	defaultMenuSize   = 8
	maxMenuSize       = 25
	singlePrefix      = "Found 1 match: "
	choicesMarker     = " matches for "
	choicesTail       = " — pick one:"
	autoLine          = "Auto-matched by katyd."
	addButtonLabel    = "Add this"
	cancelButtonLabel = "Cancel"
	expandButtonLabel = "Show more"
)

// menuOptions builds up to limit select options, skipping candidates without an id.
func menuOptions(candidates []Candidate, limit int) []discordgo.SelectMenuOption {
	options := make([]discordgo.SelectMenuOption, 0, len(candidates))
	for _, c := range candidates {
		if len(options) == limit {
			break
		}
		if c.ReleaseID == "" {
			continue
		}
		options = append(options, discordgo.SelectMenuOption{
			Label:       truncate(c.Title+" — "+c.Artist, labelLimit),
			Value:       c.ReleaseID,
			Description: candidateDetail(c),
		})
	}
	return options
}

func candidateDetail(c Candidate) string {
	parts := []string{}
	if c.Date != "" {
		parts = append(parts, c.Date)
	}
	if c.PrimaryType != "" {
		parts = append(parts, c.PrimaryType)
	}
	// release group hits carry no track count; only resolved releases do
	if c.TrackCount > 0 {
		parts = append(parts, fmt.Sprintf("%d tracks", c.TrackCount))
	}
	return truncate(strings.Join(parts, ", "), labelLimit)
}

func truncate(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit-1]) + "…"
}

// matchText renders the single-candidate message.
func matchText(c Candidate, auto bool) string {
	text := fmt.Sprintf("%s%s - %s (%s)", singlePrefix, c.Artist, c.Title, candidateDetail(c))
	if auto {
		text += "\n" + autoLine
	}
	return text
}

// choicesText renders the multi-candidate prompt. parseChoicesSpec reads it back.
func choicesText(spec addSpec, count int) string {
	text := fmt.Sprintf("Found %d%s%s - %s", count, choicesMarker, spec.Artist, spec.Album)
	if spec.Year != 0 {
		text += fmt.Sprintf(" (%d)", spec.Year)
	}
	return text + choicesTail
}

// parseMatchLabel recovers "artist - title" from a single-match message.
func parseMatchLabel(content string) string {
	text := strings.TrimPrefix(content, singlePrefix)
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	if i := strings.LastIndex(text, " ("); i > 0 {
		text = text[:i]
	}
	return strings.TrimSpace(text)
}

// parseChoicesSpec recovers the original addalbum spec from a multi-match message.
func parseChoicesSpec(content string) addSpec {
	i := strings.Index(content, choicesMarker)
	if i < 0 {
		return addSpec{}
	}
	text := strings.TrimSuffix(content[i+len(choicesMarker):], choicesTail)
	text, year := stripYear(text)
	artist, album, ok := splitArtistAlbum(text)
	if !ok {
		return addSpec{Year: year}
	}
	return addSpec{Artist: artist, Album: album, Year: year}
}

func stripYear(s string) (string, int) {
	if !strings.HasSuffix(s, ")") {
		return s, 0
	}
	i := strings.LastIndex(s, " (")
	if i < 0 {
		return s, 0
	}
	inner := s[i+2 : len(s)-1]
	year, err := strconv.Atoi(inner)
	if err != nil || len(inner) != 4 {
		return s, 0
	}
	return s[:i], year
}

func splitArtistAlbum(s string) (string, string, bool) {
	i := strings.Index(s, " - ")
	if i <= 0 {
		return "", "", false
	}
	return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+3:]), true
}
