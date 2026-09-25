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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBase     = "https://musicbrainz.org"
	requestInterval = 1100 * time.Millisecond
	maxBodyBytes    = 10 << 20
	userAgent       = "katydid/0.1.0 ( https://doppel.moe )"
	maxRetries      = 4
	maxBackoff      = 10 * time.Second
)

type Client struct {
	base     string
	hc       *http.Client
	cacheDir string
	noCache  bool
	interval time.Duration
	retries  int

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
		retries:  maxRetries,
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
		Name     string `json:"name"`
		SortName string `json:"sort-name"`
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

// CreditList returns one entry per credited artist, in credit order.
func CreditList(credits []ArtistCredit) []string {
	out := []string{}
	for _, credit := range credits {
		name := credit.Name
		if name == "" {
			name = credit.Artist.Name
		}
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// SortName returns the sort name of the first credited artist.
func SortName(credits []ArtistCredit) string {
	if len(credits) == 0 {
		return ""
	}
	return credits[0].Artist.SortName
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
	Media        []ReleaseMedia `json:"media"`
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
	Recording    struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"recording"`
}

type ReleaseMedia struct {
	Position   int            `json:"position"`
	Format     string         `json:"format"`
	TrackCount int            `json:"track-count"`
	Tracks     []ReleaseTrack `json:"tracks"`
}

type Genre struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type Release struct {
	ID           string         `json:"id"`
	Title        string         `json:"title"`
	Status       string         `json:"status"`
	Date         string         `json:"date"`
	Country      string         `json:"country"`
	ArtistCredit []ArtistCredit `json:"artist-credit"`
	ReleaseGroup *struct {
		ID               string   `json:"id"`
		PrimaryType      string   `json:"primary-type"`
		SecondaryTypes   []string `json:"secondary-types"`
		FirstReleaseDate string   `json:"first-release-date"`
	} `json:"release-group"`
	LabelInfo []LabelInfo    `json:"label-info"`
	Genres    []Genre        `json:"genres"`
	Media     []ReleaseMedia `json:"media"`
}

func (r *Release) Artist() string {
	return creditName(r.ArtistCredit)
}

// ReleaseType renders the release group's types for the RELEASETYPE tag.
func (r *Release) ReleaseType() string {
	if r.ReleaseGroup == nil {
		return ""
	}
	parts := []string{}
	if r.ReleaseGroup.PrimaryType != "" {
		parts = append(parts, r.ReleaseGroup.PrimaryType)
	}
	parts = append(parts, r.ReleaseGroup.SecondaryTypes...)
	return strings.Join(parts, ", ")
}

// IsCompilation reports whether the release is a various-artists compilation.
func (r *Release) IsCompilation() bool {
	if r.ReleaseGroup != nil {
		for _, secondary := range r.ReleaseGroup.SecondaryTypes {
			if secondary == "Compilation" {
				return true
			}
		}
	}
	return r.Artist() == "Various Artists"
}

// OriginalDate returns the release group's first release date.
func (r *Release) OriginalDate() string {
	if r.ReleaseGroup == nil {
		return ""
	}
	return r.ReleaseGroup.FirstReleaseDate
}

// TopGenres returns up to n genre names, most voted first.
func (r *Release) TopGenres(n int) []string {
	sorted := make([]Genre, len(r.Genres))
	copy(sorted, r.Genres)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Count != sorted[j].Count {
			return sorted[i].Count > sorted[j].Count
		}
		return sorted[i].Name < sorted[j].Name
	})
	out := []string{}
	for _, genre := range sorted {
		if genre.Name != "" {
			out = append(out, genre.Name)
		}
		if len(out) == n {
			break
		}
	}
	return out
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

func (c *Client) SetMaxRetries(n int) {
	c.retries = n
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
	path := "/ws/2/release/" + url.PathEscape(id) + "?inc=recordings+artist-credits+release-groups+genres&fmt=json"
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

// statusError marks a response status; 5XX is transient and retried.
type statusError struct {
	status     int
	statusText string
	retryAfter time.Duration
	requestURL string
	snippet    string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("get %s: status %s: %s", e.requestURL, e.statusText, e.snippet)
}

func retryAfter(header http.Header) time.Duration {
	raw := header.Get("Retry-After")
	if raw == "" {
		return 0
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// fetch retries transient failures (5XX, dropped connections) with
// exponential backoff derived from the request interval; musicbrainz
// throws occasional 503s under load and a bare failure would skip the
// album. Retry-After is honored when larger than the computed backoff.
func (c *Client) fetch(ctx context.Context, requestURL string) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		body, err := c.attempt(ctx, requestURL)
		if err == nil {
			return body, nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		var status *statusError
		transient := !errors.As(err, &status) || status.status >= 500
		if !transient || attempt >= c.retries {
			return nil, err
		}
		wait := c.backoff(attempt, status)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

func (c *Client) attempt(ctx context.Context, requestURL string) ([]byte, error) {
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
		return nil, &statusError{
			status:     resp.StatusCode,
			statusText: resp.Status,
			retryAfter: retryAfter(resp.Header),
			requestURL: requestURL,
			snippet:    snippet,
		}
	}
	return body, nil
}

func (c *Client) backoff(attempt int, status *statusError) time.Duration {
	wait := c.interval << attempt
	if wait > maxBackoff {
		wait = maxBackoff
	}
	if status != nil && status.retryAfter > wait {
		wait = status.retryAfter
	}
	if wait > maxBackoff {
		wait = maxBackoff
	}
	return wait
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

// CreditName renders an artist-credit as a display string.
func CreditName(credits []ArtistCredit) string {
	return creditName(credits)
}
