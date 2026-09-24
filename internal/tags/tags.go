package tags

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	taglib "go.senan.xyz/taglib"
)

type Tags struct {
	Title          string   `json:"title"`
	Artists        []string `json:"artists"`
	AlbumArtists   []string `json:"album_artists"`
	Album          string   `json:"album"`
	TrackNumber    int      `json:"track_number"`
	TrackTotal     int      `json:"track_total"`
	DiscNumber     int      `json:"disc_number"`
	DiscTotal      int      `json:"disc_total"`
	Year           int      `json:"year"`
	OriginalYear   int      `json:"original_year"`
	Genres         []string `json:"genres"`
	Label          string   `json:"label"`
	CatalogNumber  string   `json:"catalog_number"`
	ReleaseGroupID string   `json:"release_group_id"`
	ReleaseID      string   `json:"release_id"`
	LengthSeconds  float64  `json:"length_seconds"`
	SampleRate     int      `json:"sample_rate"`
	Bitrate        int      `json:"bitrate"`
	BitDepth       int      `json:"bit_depth"`
	Channels       int      `json:"channels"`
}

func Read(path string) (Tags, error) {
	raw, err := taglib.ReadTags(path)
	if err != nil {
		return Tags{}, fmt.Errorf("read tags %s: %w", path, err)
	}
	props, err := taglib.ReadProperties(path)
	if err != nil {
		return Tags{}, fmt.Errorf("read properties %s: %w", path, err)
	}

	trackNum, trackTotal := pair(raw[taglib.TrackNumber])
	discNum, discTotal := pair(raw[taglib.DiscNumber])

	t := Tags{
		Title:          first(raw[taglib.Title]),
		Artists:        all(raw[taglib.Artist]),
		AlbumArtists:   all(raw[taglib.AlbumArtist]),
		Album:          first(raw[taglib.Album]),
		TrackNumber:    trackNum,
		TrackTotal:     trackTotal,
		DiscNumber:     discNum,
		DiscTotal:      discTotal,
		Year:           year(raw[taglib.Date]),
		OriginalYear:   year(raw[taglib.OriginalDate]),
		Genres:         all(raw[taglib.Genre]),
		Label:          first(raw[taglib.Label]),
		CatalogNumber:  first(raw[taglib.CatalogNumber]),
		ReleaseGroupID: first(raw[taglib.MusicBrainzReleaseGroupID]),
		ReleaseID:      first(raw[taglib.MusicBrainzAlbumID]),
		LengthSeconds:  props.Length.Seconds(),
		SampleRate:     int(props.SampleRate),
		Bitrate:        int(props.BitRate),
		BitDepth:       int(props.BitDepth),
		Channels:       int(props.Channels),
	}
	return t, nil
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

func all(values []string) []string {
	out := []string{}
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func year(values []string) int {
	date := first(values)
	if date == "" {
		return 0
	}
	return parseYear(date)
}

func parseYear(date string) int {
	if y, err := strconv.Atoi(date[:4]); err == nil && len(date) >= 4 {
		return y
	}
	if t, err := time.Parse(time.RFC3339, date); err == nil {
		return t.Year()
	}
	return 0
}

func pair(values []string) (num, total int) {
	value := first(values)
	if value == "" {
		return 0, 0
	}
	before, after, found := strings.Cut(value, "/")
	num = atoi(before)
	if found {
		total = atoi(after)
	}
	return num, total
}

func atoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}
