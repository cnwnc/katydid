package mb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	defaultBase     = "https://musicbrainz.org"
	requestInterval = 1100 * time.Millisecond
	maxBodyBytes    = 10 << 20
	userAgent       = "katydid/0.1.0 ( https://doppel.moe )"
)

type Client struct {
	base     string
	hc       *http.Client
	cacheDir string
	noCache  bool
	interval time.Duration

	mu          sync.Mutex
	lastRequest time.Time
}

func New(base, cacheDir string, noCache bool) *Client {
	if base == "" {
		base = defaultBase
	}
	base = strings.TrimSuffix(base, "/")
	return &Client{
		base:     base,
		hc:       &http.Client{Timeout: 30 * time.Second},
		cacheDir: cacheDir,
		noCache:  noCache,
		interval: requestInterval,
	}
}

func BuildQuery(artist, album string) string {
	return fmt.Sprintf("artist:%s AND release:%s", quoteTerm(artist), quoteTerm(album))
}

func quoteTerm(value string) string {
	escaped := strings.ReplaceAll(value, "\\", "\\\\")
	escaped = strings.ReplaceAll(escaped, "\"", "\\\"")
	return "\"" + escaped + "\""
}

type ArtistCredit struct {
	Name       string `json:"name"`
	Joinphrase string `json:"joinphrase"`
	Artist     struct {
		Name string `json:"name"`
	} `json:"artist"`
}

func creditName(credits []ArtistCredit) string {
	var builder strings.Builder
	for _, credit := range credits {
		name := credit.Name
		if name == "" {
			name = credit.Artist.Name
		}
		builder.WriteString(name)
		builder.WriteString(credit.Joinphrase)
	}
	return strings.TrimSpace(builder.String())
}

type labelRef struct {
	Name string `json:"name"`
}

type LabelInfo struct {
	CatalogNumber string    `json:"catalog-number"`
	Label         *labelRef `json:"label"`
}

type SearchRelease struct {
	ID           string         `json:"id"`
	Score        int            `json:"score"`
	Title        string         `json:"title"`
	Status       string         `json:"status"`
	Date         string         `json:"date"`
	Country      string         `json:"country"`
	TrackCount   int            `json:"track-count"`
	ArtistCredit []ArtistCredit `json:"artist-credit"`
	ReleaseGroup *struct {
		ID          string `json:"id"`
		Title       string `json:"title"`
		PrimaryType string `json:"primary-type"`
	} `json:"release-group"`
	LabelInfo []LabelInfo `json:"label-info"`
}

func (r *SearchRelease) Artist() string {
	return creditName(r.ArtistCredit)
}

type ReleaseTrack struct {
	ID           string         `json:"id"`
	Position     int            `json:"position"`
	Number       string         `json:"number"`
	Title        string         `json:"title"`
	Length       int64          `json:"length"`
	ArtistCredit []ArtistCredit `json:"artist-credit"`
}

type ReleaseMedia struct {
	Position   int            `json:"position"`
	Format     string         `json:"format"`
	TrackCount int            `json:"track-count"`
	Tracks     []ReleaseTrack `json:"tracks"`
}

type Release struct {
	ID           string         `json:"id"`
	Title        string         `json:"title"`
	Status       string         `json:"status"`
	Date         string         `json:"date"`
	Country      string         `json:"country"`
	ArtistCredit []ArtistCredit `json:"artist-credit"`
	ReleaseGroup *struct {
		ID           string   `json:"id"`
		PrimaryType  string   `json:"primary-type"`
		SecondaryIDs []string `json:"secondary-type-ids"`
	} `json:"release-group"`
	LabelInfo []LabelInfo    `json:"label-info"`
	Media     []ReleaseMedia `json:"media"`
}

func (r *Release) Artist() string {
	return creditName(r.ArtistCredit)
}

func (r *Release) FlattenedTracks() []ReleaseTrack {
	media := make([]ReleaseMedia, len(r.Media))
	copy(media, r.Media)
	for i := 1; i < len(media); i++ {
		for j := i; j > 0 && media[j].Position < media[j-1].Position; j-- {
			media[j], media[j-1] = media[j-1], media[j]
		}
	}
	tracks := []ReleaseTrack{}
	for _, medium := range media {
		mediumTracks := make([]ReleaseTrack, len(medium.Tracks))
		copy(mediumTracks, medium.Tracks)
		for i := 1; i < len(mediumTracks); i++ {
			for j := i; j > 0 && mediumTracks[j].Position < mediumTracks[j-1].Position; j-- {
				mediumTracks[j], mediumTracks[j-1] = mediumTracks[j-1], mediumTracks[j]
			}
		}
		tracks = append(tracks, mediumTracks...)
	}
	return tracks
}

func (c *Client) SetRequestInterval(d time.Duration) {
	c.interval = d
}

func (c *Client) SearchReleases(ctx context.Context, query string) ([]SearchRelease, error) {
	path := "/ws/2/release?query=" + url.QueryEscape(query) + "&fmt=json&limit=25"
	var out struct {
		Releases []SearchRelease `json:"releases"`
	}
	if err := c.get(ctx, path, &out); err != nil {
		return nil, fmt.Errorf("search releases: %w", err)
	}
	return out.Releases, nil
}

func (c *Client) LookupRelease(ctx context.Context, id string) (*Release, error) {
	path := "/ws/2/release/" + url.PathEscape(id) + "?inc=recordings+artist-credits+release-groups&fmt=json"
	var out Release
	if err := c.get(ctx, path, &out); err != nil {
		return nil, fmt.Errorf("lookup release %s: %w", id, err)
	}
	return &out, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	requestURL := c.base + path
	body, cached, err := c.read(requestURL)
	if err != nil {
		return err
	}
	if !cached {
		body, err = c.fetch(ctx, requestURL)
		if err != nil {
			return err
		}
		c.write(requestURL, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func (c *Client) fetch(ctx context.Context, requestURL string) ([]byte, error) {
	c.throttle()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request %s: %w", requestURL, err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", requestURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", requestURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 120 {
			snippet = snippet[:120]
		}
		return nil, fmt.Errorf("get %s: status %s: %s", requestURL, resp.Status, snippet)
	}
	return body, nil
}

func (c *Client) throttle() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if wait := c.interval - time.Since(c.lastRequest); wait > 0 {
		time.Sleep(wait)
	}
	c.lastRequest = time.Now()
}

func (c *Client) read(requestURL string) (body []byte, cached bool, err error) {
	if c.cacheDir == "" || c.noCache {
		return nil, false, nil
	}
	data, err := os.ReadFile(c.cachePath(requestURL))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, nil
	}
	return data, true, nil
}

func (c *Client) write(requestURL string, body []byte) {
	if c.cacheDir == "" {
		return
	}
	path := c.cachePath(requestURL)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return
	}
	os.Rename(tmp, path)
}

func (c *Client) cachePath(requestURL string) string {
	sum := sha256.Sum256([]byte(requestURL))
	return filepath.Join(c.cacheDir, hex.EncodeToString(sum[:])+".json")
}
