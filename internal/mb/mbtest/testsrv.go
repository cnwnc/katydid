// Package mbtest provides musicbrainz http test servers for other packages.
package mbtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"doppel.moe/katydid/internal/mb"
)

// Custom serves whatever the handler returns.
func Custom(t *testing.T, respond func(r *http.Request) (body string, status int)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, status := respond(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func SyntheticSearch(id, groupID, title, artist, date string, trackCount int) mb.SearchRelease {
	release := mb.SearchRelease{
		ID:         id,
		Score:      100,
		Title:      title,
		Status:     "Official",
		Date:       date,
		TrackCount: trackCount,
	}
	release.ReleaseGroup = &struct {
		ID          string `json:"id"`
		Title       string `json:"title"`
		PrimaryType string `json:"primary-type"`
	}{ID: groupID, Title: title, PrimaryType: "Album"}
	release.ArtistCredit = []mb.ArtistCredit{{Name: artist}}
	return release
}

func SyntheticRelease(id, groupID, title, artist, date string, media ...mb.ReleaseMedia) mb.Release {
	release := mb.Release{
		ID:     id,
		Title:  title,
		Status: "Official",
		Date:   date,
	}
	release.ReleaseGroup = &struct {
		ID               string   `json:"id"`
		PrimaryType      string   `json:"primary-type"`
		SecondaryTypes   []string `json:"secondary-types"`
		FirstReleaseDate string   `json:"first-release-date"`
	}{ID: groupID, PrimaryType: "Album"}
	release.ArtistCredit = []mb.ArtistCredit{{Name: artist}}
	release.Media = media
	return release
}

func Track(position int, title string) mb.ReleaseTrack {
	track := mb.ReleaseTrack{ID: "trk-" + title, Position: position, Number: fmt.Sprint(position), Title: title, Length: 60000}
	track.Recording.ID = "rec-" + title
	track.Recording.Title = title
	track.ArtistCredit = []mb.ArtistCredit{{Name: ""}}
	track.ArtistCredit[0].Artist.Name = ""
	return track
}

// JSON marshals any value for inline fixture bodies.
func JSON(v any) string {
	out, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(out)
}
