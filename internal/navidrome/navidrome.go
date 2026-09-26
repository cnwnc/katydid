package navidrome

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrScanDenied means the navidrome user may not trigger a scan; the
// refresh then only waits for an already running scan and reads the
// library as it is.
var ErrScanDenied = errors.New("navidrome user is not allowed to trigger a scan")

const (
	subsonicVer    = "1.16.1"
	clientName     = "katydid"
	scanTick       = 2 * time.Second
	scanWaitCap    = 90 * time.Second
	requestTimeout = 15 * time.Second
)

// Client talks to a navidrome instance: it triggers a library scan after
// a katydid import, finds the imported album, and mints a share link
// with downloads enabled. Subsonic endpoints authenticate per request;
// the native share API needs a bearer token from /auth/login.
type Client struct {
	base     string
	username string
	password string
	hc       *http.Client

	mu    sync.Mutex
	token string
}

func New(base, username, password string) *Client {
	return &Client{
		base:     strings.TrimRight(base, "/"),
		username: username,
		password: password,
		hc:       &http.Client{Timeout: requestTimeout},
	}
}

// BaseURL resolves the server address: the default is http://127.0.0.1
// (not localhost, which may resolve to ::1 first), base overrides scheme
// and host, port overrides the port. A base that already carries a port
// wins unless port is also given.
func BaseURL(base, port string) (string, error) {
	if base == "" {
		base = "http://127.0.0.1"
	}
	parsed, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return "", fmt.Errorf("parse navidrome base url %q: %w", base, err)
	}
	if parsed.Scheme == "" || parsed.Hostname() == "" {
		return "", fmt.Errorf("navidrome base url %q needs scheme and host", base)
	}
	switch {
	case port != "":
		parsed.Host = parsed.Hostname() + ":" + port
	case parsed.Port() == "":
		parsed.Host = parsed.Host + ":4533"
	}
	return parsed.String(), nil
}

// RefreshAndShare scans the library, finds the album, creates a share
// for it with downloads enabled, and returns the public share URL.
func (c *Client) RefreshAndShare(ctx context.Context, artist, album string) (string, error) {
	if err := c.refresh(ctx); err != nil {
		return "", err
	}
	albumID, err := c.findAlbum(ctx, artist, album)
	if err != nil {
		return "", err
	}
	share, err := c.createShare(ctx, albumID, artist+" - "+album)
	if err != nil {
		return "", err
	}
	if err := c.enableDownload(ctx, share.ID); err != nil {
		return "", err
	}
	if share.URL == "" {
		share.URL = c.base + "/share/" + share.ID
	}
	return share.URL, nil
}

// refresh triggers a scan and waits for it to finish. Without scan
// rights it still waits out a scan someone else started, then gives up
// waiting; the search runs on the library as it stands.
func (c *Client) refresh(ctx context.Context) error {
	err := c.triggerScan(ctx)
	wait := true
	if errors.Is(err, ErrScanDenied) {
		scanning, statusErr := c.scanStatus(ctx)
		if statusErr != nil {
			return statusErr
		}
		wait = scanning
	} else if err != nil {
		return err
	}
	if !wait {
		return nil
	}
	return c.awaitScan(ctx)
}

func (c *Client) triggerScan(ctx context.Context) error {
	var out struct {
		SubsonicResponse struct {
			Status string `json:"status"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"subsonic-response"`
	}
	if err := c.subsonic(ctx, "startScan", nil, &out); err != nil {
		return err
	}
	if out.SubsonicResponse.Error != nil {
		e := out.SubsonicResponse.Error
		if e.Code == 50 {
			return ErrScanDenied
		}
		return fmt.Errorf("navidrome startScan: %s", e.Message)
	}
	return nil
}

func (c *Client) scanStatus(ctx context.Context) (bool, error) {
	var out struct {
		SubsonicResponse struct {
			ScanStatus struct {
				Scanning bool `json:"scanning"`
			} `json:"scanStatus"`
		} `json:"subsonic-response"`
	}
	if err := c.subsonic(ctx, "getScanStatus", nil, &out); err != nil {
		return false, err
	}
	return out.SubsonicResponse.ScanStatus.Scanning, nil
}

// awaitScan polls the scan status until a started scan finishes. A scan
// that ends before the first poll lands reads as done immediately.
func (c *Client) awaitScan(ctx context.Context) error {
	deadline := time.Now().Add(scanWaitCap)
	seen := false
	for {
		scanning, err := c.scanStatus(ctx)
		if err != nil {
			return err
		}
		if scanning {
			seen = true
		} else if seen || time.Now().After(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(scanTick):
		}
	}
}

func (c *Client) findAlbum(ctx context.Context, artist, album string) (string, error) {
	params := url.Values{}
	params.Set("query", album)
	params.Set("albumCount", "20")
	var out struct {
		SubsonicResponse struct {
			SearchResult3 struct {
				Albums []struct {
					ID     string `json:"id"`
					Name   string `json:"name"`
					Artist string `json:"artist"`
				} `json:"album"`
			} `json:"searchResult3"`
		} `json:"subsonic-response"`
	}
	if err := c.subsonic(ctx, "search3", params, &out); err != nil {
		return "", err
	}
	wantAlbum, wantArtist := loose(album), loose(artist)
	for _, a := range out.SubsonicResponse.SearchResult3.Albums {
		if loose(a.Name) == wantAlbum && loose(a.Artist) == wantArtist {
			return a.ID, nil
		}
	}
	return "", fmt.Errorf("album %q by %q not found in navidrome", album, artist)
}

type share struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

func (c *Client) createShare(ctx context.Context, albumID, description string) (share, error) {
	params := url.Values{}
	params.Set("id", albumID)
	params.Set("description", description)
	var out struct {
		SubsonicResponse struct {
			Status string `json:"status"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
			Shares struct {
				Share []share `json:"share"`
			} `json:"shares"`
		} `json:"subsonic-response"`
	}
	if err := c.subsonic(ctx, "createShare", params, &out); err != nil {
		return share{}, err
	}
	if out.SubsonicResponse.Error != nil {
		return share{}, fmt.Errorf("navidrome createShare: %s", out.SubsonicResponse.Error.Message)
	}
	shares := out.SubsonicResponse.Shares.Share
	if len(shares) == 0 {
		return share{}, errors.New("navidrome createShare returned no share")
	}
	return shares[0], nil
}

// enableDownload flips the share's downloadable flag via the native
// API, the only place navidrome exposes it.
func (c *Client) enableDownload(ctx context.Context, shareID string) error {
	body := strings.NewReader(`{"downloadable":true}`)
	code, err := c.native(ctx, http.MethodPut, "/api/share/"+url.PathEscape(shareID), body)
	if err == nil {
		return nil
	}
	if code != http.StatusUnauthorized {
		return fmt.Errorf("enable share downloads: %w", err)
	}
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
	if _, err := c.native(ctx, http.MethodPut, "/api/share/"+url.PathEscape(shareID), body); err != nil {
		return fmt.Errorf("enable share downloads: %w", err)
	}
	return nil
}

func (c *Client) authToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	token := c.token
	c.mu.Unlock()
	if token != "" {
		return token, nil
	}
	payload, err := json.Marshal(map[string]string{"username": c.username, "password": c.password})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/auth/login", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("navidrome login: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("navidrome login: %s", strings.TrimSpace(string(raw)))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" {
		return "", fmt.Errorf("navidrome login response: %w", err)
	}
	c.mu.Lock()
	c.token = out.Token
	c.mu.Unlock()
	return out.Token, nil
}

// native calls a bearer-authenticated endpoint and returns the http
// status alongside any error so callers can retry after re-login.
func (c *Client) native(ctx context.Context, method, path string, body io.Reader) (int, error) {
	token, err := c.authToken(ctx)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("x-nd-authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("navidrome %s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode/100 != 2 {
		return res.StatusCode, fmt.Errorf("%s: %s", res.Status, strings.TrimSpace(string(raw)))
	}
	return res.StatusCode, nil
}

func (c *Client) subsonic(ctx context.Context, endpoint string, params url.Values, out any) error {
	if params == nil {
		params = url.Values{}
	}
	params.Set("u", c.username)
	params.Set("p", c.password)
	params.Set("v", subsonicVer)
	params.Set("c", clientName)
	params.Set("f", "json")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/rest/"+endpoint+"?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("navidrome %s: %w", endpoint, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("navidrome %s: %s", endpoint, res.Status)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("navidrome %s response: %w", endpoint, err)
	}
	return nil
}

// loose folds a name for comparison: letters and digits, lowercased.
func loose(s string) string {
	var b strings.Builder
	for _, ch := range strings.ToLower(s) {
		if ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' {
			b.WriteRune(ch)
		}
	}
	return b.String()
}
