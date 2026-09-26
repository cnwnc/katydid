package lastfm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"doppel.moe/katydid/internal/match"
)

const (
	defaultBase  = "https://ws.audioscrobbler.com/2.0/"
	maxBodyBytes = 1 << 20
)

var ErrNotFound = errors.New("last.fm album not found")

type Track struct {
	Name     string
	Duration int // seconds, 0 when last.fm has none (it often does)
	Rank     int // 1-based position
}

type Album struct {
	Name      string
	Artist    string
	MBID      string // often empty; that is meaningful, not an error
	URL       string // last.fm album url
	Listeners int
	Tracks    []Track
}

// Client is a stateless last.fm web api client; safe for concurrent use.
type Client struct {
	base string
	key  string
	hc   *http.Client
}

func New(key string) *Client {
	return newWithBase(defaultBase, key)
}

func newWithBase(base, key string) *Client {
	return &Client{
		base: base,
		key:  key,
		hc:   &http.Client{Timeout: 15 * time.Second},
	}
}

// SetBase repoints the client at a custom endpoint, mirroring the
// mb.Client test hooks so other packages' tests can stub the API.
func (c *Client) SetBase(base string) {
	c.base = base
}

// GetInfo is the exact (case-insensitive) artist+album lookup. Returns
// ErrNotFound when last.fm answers error code 6.
func (c *Client) GetInfo(ctx context.Context, artist, album string) (Album, error) {
	params := url.Values{
		"method": {"album.getinfo"},
		"artist": {artist},
		"album":  {album},
	}
	var out apiAlbumResponse
	if err := c.get(ctx, params, &out); err != nil {
		return Album{}, err
	}
	found := Album{
		Name:      out.Album.Name,
		Artist:    out.Album.Artist,
		MBID:      out.Album.MBID,
		URL:       out.Album.URL,
		Listeners: atoi(out.Album.Listeners),
	}
	// stub pages carry no tracks key; that is fine, not an error
	if out.Album.Tracks != nil {
		for _, t := range out.Album.Tracks.Track {
			found.Tracks = append(found.Tracks, Track{
				Name:     t.Name,
				Duration: atoi(t.Duration),
				Rank:     atoi(t.Attr.Rank),
			})
		}
	}
	return found, nil
}

// Search runs album.search (fuzzy, title-anchored) and returns hits
// ranked by combined artist+title similarity to the query, best first.
// Used only to recover the exact spelling of an album last.fm knows;
// callers re-lookup with GetInfo or take the entry's mbid.
func (c *Client) Search(ctx context.Context, artist, album string) ([]Album, error) {
	params := url.Values{
		"method": {"album.search"},
		"artist": {artist},
		"album":  {album},
	}
	var out apiSearchResponse
	if err := c.get(ctx, params, &out); err != nil {
		return nil, err
	}
	hits := out.Results.AlbumMatches.Album
	albums := make([]Album, 0, len(hits))
	for _, h := range hits {
		albums = append(albums, Album{
			Name:   h.Name,
			Artist: h.Artist,
			MBID:   h.MBID,
			URL:    h.URL,
		})
	}
	// album.search hits have no listeners field; the caller filters
	sims := make([]float64, len(albums))
	for i, a := range albums {
		sims[i] = (match.Similarity(a.Name, album) + 2*match.Similarity(a.Artist, artist)) / 3
	}
	sort.SliceStable(albums, func(i, j int) bool {
		if sims[i] != sims[j] {
			return sims[i] > sims[j]
		}
		if albums[i].Artist != albums[j].Artist {
			return albums[i].Artist < albums[j].Artist
		}
		return albums[i].Name < albums[j].Name
	})
	return albums, nil
}

// get performs one api call; last.fm reports failures in-band as
// {"error":6,"message":...} with a 200 status, so the envelope is
// probed and mapped before the payload is decoded into out.
func (c *Client) get(ctx context.Context, params url.Values, out any) error {
	params.Set("api_key", c.key)
	params.Set("format", "json")
	body, err := c.fetch(ctx, params)
	if err != nil {
		return err
	}
	var probe apiError
	if err := json.Unmarshal(body, &probe); err != nil {
		return fmt.Errorf("last.fm: decode: %w", err)
	}
	switch {
	case probe.Code == 6:
		return ErrNotFound
	case probe.Code != 0:
		return fmt.Errorf("last.fm error %d: %s", probe.Code, probe.Message)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("last.fm: decode: %w", err)
	}
	return nil
}

func (c *Client) fetch(ctx context.Context, params url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"?"+params.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("last.fm: build request: %w", err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("last.fm: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("last.fm: read: %w", err)
	}
	return body, nil
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// apiError is last.fm's in-band error envelope; a zero code means success.
type apiError struct {
	Code    int    `json:"error"`
	Message string `json:"message"`
}

type apiTrackAttr struct {
	Rank string `json:"rank"`
}

type apiTrack struct {
	Name     string       `json:"name"`
	Duration string       `json:"duration"`
	Attr     apiTrackAttr `json:"@attr"`
}

type apiAlbum struct {
	Name      string `json:"name"`
	Artist    string `json:"artist"`
	MBID      string `json:"mbid"`
	URL       string `json:"url"`
	Listeners string `json:"listeners"`
	Tracks    *struct {
		Track []apiTrack `json:"track"`
	} `json:"tracks"`
}

type apiSearchHit struct {
	Name   string `json:"name"`
	Artist string `json:"artist"`
	URL    string `json:"url"`
	MBID   string `json:"mbid"`
}

type apiAlbumResponse struct {
	Album apiAlbum `json:"album"`
}

type apiSearchResponse struct {
	Results struct {
		AlbumMatches struct {
			Album []apiSearchHit `json:"album"`
		} `json:"albummatches"`
	} `json:"results"`
}
