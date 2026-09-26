package importer

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"doppel.moe/katydid/internal/lastfm"
	"doppel.moe/katydid/internal/match"
	"doppel.moe/katydid/internal/mb"
)

const (
	sourceLastFM  = "lastfm"
	lastfmDirMark = " [last.fm]"
	lastfmComment = "metadata from last.fm; not musicbrainz-vetted"
)

// importLastFM imports from a last.fm-sourced release: musicbrainz has
// no entry, so identity comes from last.fm names and every artifact is
// marked as unvetted. The lookup is redone here so the tags always
// carry the authoritative last.fm data, not what the caller typed.
func (m *Manager) importLastFM(ctx context.Context, req Request, files []sourceFile, evidence match.Evidence) (*Result, error) {
	if req.Artist == "" || req.Album == "" {
		return nil, errors.New("last.fm import needs artist and album")
	}
	if m.lastfm == nil {
		return nil, errors.New("last.fm is not configured")
	}
	al, err := m.lastfm.GetInfo(ctx, req.Artist, req.Album)
	if err != nil {
		if errors.Is(err, lastfm.ErrNotFound) {
			return nil, fmt.Errorf("no last.fm album for %s - %s: %w", req.Artist, req.Album, lastfm.ErrNotFound)
		}
		return nil, err
	}

	// the fetched entry wins over whatever the caller typed, so the
	// sidecar and notes carry the authoritative last.fm url
	req.URL = al.URL

	release := &mb.Release{ID: "", Title: al.Name, Status: "Official"}
	credit := mb.ArtistCredit{Name: al.Artist}
	credit.Artist.Name = al.Artist
	release.ArtistCredit = []mb.ArtistCredit{credit}
	media := mb.ReleaseMedia{Position: 1, Format: "Digital Media", TrackCount: len(al.Tracks)}
	for i, t := range al.Tracks {
		media.Tracks = append(media.Tracks, mb.ReleaseTrack{
			ID: "", Position: i + 1, Number: strconv.Itoa(i + 1), Title: t.Name, Length: int64(t.Duration) * 1000,
		})
	}
	release.Media = []mb.ReleaseMedia{media}

	albumID, notes, err := m.publish(req, release, files, nil, false)
	if err != nil {
		return nil, err
	}
	if al.URL != "" {
		notes = append(notes, "metadata sourced from last.fm, not musicbrainz-vetted: "+al.URL)
	}
	result := Result{Status: statusImported, AlbumID: albumID, Format: formatLabel(release), Notes: notes}
	m.record(req.Request, result)
	return &result, nil
}
