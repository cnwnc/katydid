package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const clientTimeout = 10 * time.Second

type Katyd struct{ hc *http.Client }

func NewKatyd(socket string) *Katyd {
	return &Katyd{hc: dialSocket(socket)}
}

type Fetchd struct{ hc *http.Client }

func NewFetchd(socket string) *Fetchd {
	return &Fetchd{hc: dialSocket(socket)}
}

func dialSocket(path string) *http.Client {
	return &http.Client{
		Timeout: clientTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		},
	}
}

// Resolve returns the katyd release group candidates for a specifier.
func (k *Katyd) Resolve(q Query) ([]Candidate, bool, error) {
	params := url.Values{}
	params.Set("artist", q.Artist)
	params.Set("album", q.Album)
	if q.Year != 0 {
		params.Set("year", strconv.Itoa(q.Year))
	}
	if q.MBID != "" {
		params.Set("mbid", q.MBID)
	}
	params.Set("limit", strconv.Itoa(q.Limit))
	var out resolveResponse
	if err := request(k.hc, http.MethodGet, "/resolve?"+params.Encode(), nil, &out); err != nil {
		return nil, false, fmt.Errorf("katyd resolve: %w", err)
	}
	return out.Candidates, out.Auto, nil
}

// Add queues a want on fetchd.
func (f *Fetchd) Add(a AddWant) (Want, error) {
	var out wantResponse
	if err := request(f.hc, http.MethodPost, "/wants", a, &out); err != nil {
		return Want{}, fmt.Errorf("fetchd add want: %w", err)
	}
	return out.Want, nil
}

// Want fetches one want by id.
func (f *Fetchd) Want(id string) (Want, error) {
	var out wantResponse
	if err := request(f.hc, http.MethodGet, "/want?id="+url.QueryEscape(id), nil, &out); err != nil {
		return Want{}, fmt.Errorf("fetchd get want: %w", err)
	}
	return out.Want, nil
}

// Wants lists all wants, oldest first.
func (f *Fetchd) Wants() ([]Want, error) {
	var out wantsResponse
	if err := request(f.hc, http.MethodGet, "/wants", nil, &out); err != nil {
		return nil, fmt.Errorf("fetchd list wants: %w", err)
	}
	return out.Wants, nil
}

func request(hc *http.Client, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, "http://unix"+path, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", resp.Status, errorText(raw))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func errorText(raw []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &e); err == nil && e.Error != "" {
		return e.Error
	}
	return strings.TrimSpace(string(raw))
}
