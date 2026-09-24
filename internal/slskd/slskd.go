// Package slskd is a client for the slskd HTTP API.
package slskd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxBodyBytes = 10 << 20

type Client struct {
	base   string
	apiKey string
	hc     *http.Client
}

func New(base, apiKey string) *Client {
	return &Client{
		base:   strings.TrimSuffix(base, "/"),
		apiKey: apiKey,
		hc:     &http.Client{Timeout: 30 * time.Second},
	}
}

type SearchRequest struct {
	SearchText                 string `json:"searchText"`
	ID                         string `json:"id,omitempty"`
	FileLimit                  int    `json:"fileLimit,omitempty"`
	FilterResponses            *bool  `json:"filterResponses,omitempty"`
	MaximumPeerQueueLength     int    `json:"maximumPeerQueueLength,omitempty"`
	MinimumPeerUploadSpeed     int    `json:"minimumPeerUploadSpeed,omitempty"`
	MinimumResponseFileCount   int    `json:"minimumResponseFileCount,omitempty"`
	ResponseLimit              int    `json:"responseLimit,omitempty"`
	SearchTimeout              int64  `json:"searchTimeout,omitempty"`
}

type Search struct {
	ID               string     `json:"id"`
	SearchText       string     `json:"searchText"`
	State            string     `json:"state"`
	StartedAt        time.Time  `json:"startedAt"`
	EndedAt          *time.Time `json:"endedAt"`
	IsComplete       bool       `json:"isComplete"`
	FileCount        int        `json:"fileCount"`
	LockedFileCount  int        `json:"lockedFileCount"`
	ResponseCount    int        `json:"responseCount"`
	Token            int64      `json:"token"`
	Responses        []Response `json:"responses"`
}

type Response struct {
	Username           string `json:"username"`
	Token              int64  `json:"token"`
	HasFreeUploadSlot  bool   `json:"hasFreeUploadSlot"`
	UploadSpeed        int64  `json:"uploadSpeed"`
	QueueLength        int64  `json:"queueLength"`
	FileCount          int    `json:"fileCount"`
	LockedFileCount    int    `json:"lockedFileCount"`
	Files              []File `json:"files"`
	LockedFiles        []File `json:"lockedFiles"`
}

type File struct {
	Filename         string  `json:"filename"`
	Size             int64   `json:"size"`
	Extension        string  `json:"extension"`
	BitRate          *int    `json:"bitRate"`
	BitDepth         *int    `json:"bitDepth"`
	SampleRate       *int    `json:"sampleRate"`
	IsVariableBitRate *bool  `json:"isVariableBitRate"`
	Length           *int64  `json:"length"`
	IsLocked         bool    `json:"isLocked"`
	Code             int     `json:"code"`
}

type QueueDownloadRequest struct {
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
}

type UserResponse struct {
	Username    string              `json:"username"`
	Directories []DirectoryResponse `json:"directories"`
}

type DirectoryResponse struct {
	Directory string     `json:"directory"`
	FileCount int        `json:"fileCount"`
	Files     []Transfer `json:"files"`
}

type Transfer struct {
	ID               string     `json:"id"`
	Username         string     `json:"username"`
	Direction        string     `json:"direction"`
	Filename         string     `json:"filename"`
	Size             int64      `json:"size"`
	State            string     `json:"state"`
	RequestedAt      time.Time  `json:"requestedAt"`
	EnqueuedAt       *time.Time `json:"enqueuedAt"`
	StartedAt        *time.Time `json:"startedAt"`
	EndedAt          *time.Time `json:"endedAt"`
	BytesTransferred int64      `json:"bytesTransferred"`
	AverageSpeed     float64    `json:"averageSpeed"`
	PlaceInQueue     *int64     `json:"placeInQueue"`
	Exception        *string    `json:"exception"`
	Attempts         int        `json:"attempts"`
	NextAttemptAt    *time.Time `json:"nextAttemptAt"`
	Removed          bool       `json:"removed"`
	BytesRemaining   int64      `json:"bytesRemaining"`
	PercentComplete  float64    `json:"percentComplete"`
	RemainingTime    *int64     `json:"remainingTime"`
}

func (c *Client) CreateSearch(ctx context.Context, req SearchRequest) (*Search, error) {
	var out Search
	if err := c.do(ctx, http.MethodPost, "/api/v0/searches", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Search(ctx context.Context, id string, includeResponses bool) (*Search, error) {
	path := "/api/v0/searches/" + url.PathEscape(id)
	if includeResponses {
		path += "?includeResponses=true"
	}
	var out Search
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Searches(ctx context.Context) ([]Search, error) {
	var out []Search
	if err := c.do(ctx, http.MethodGet, "/api/v0/searches", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) CancelSearch(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPut, "/api/v0/searches/"+url.PathEscape(id), nil, nil)
}

func (c *Client) DeleteSearch(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/v0/searches/"+url.PathEscape(id), nil, nil)
}

func (c *Client) EnqueueDownloads(ctx context.Context, username string, files []File) error {
	reqs := make([]QueueDownloadRequest, 0, len(files))
	for _, file := range files {
		reqs = append(reqs, QueueDownloadRequest{Filename: file.Filename, Size: file.Size})
	}
	return c.do(ctx, http.MethodPost, "/api/v0/transfers/downloads/"+url.PathEscape(username), reqs, nil)
}

func (c *Client) Downloads(ctx context.Context) ([]UserResponse, error) {
	var out []UserResponse
	if err := c.do(ctx, http.MethodGet, "/api/v0/transfers/downloads", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) RemoveDownload(ctx context.Context, username, id string, remove bool) error {
	path := fmt.Sprintf("/api/v0/transfers/downloads/%s/%s?remove=%t", url.PathEscape(username), url.PathEscape(id), remove)
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		request.Header.Set("X-API-Key", c.apiKey)
	}
	response, err := c.hc.Do(request)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("%s %s: read body: %w", method, path, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%s %s: status %d: %s", method, path, response.StatusCode, strings.TrimSpace(string(payload)))
	}
	if out == nil || len(payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}

// StateFlags splits a slskd transfer state string into its flag names.
func StateFlags(state string) []string {
	flags := []string{}
	for _, flag := range strings.Split(state, ",") {
		if flag = strings.TrimSpace(flag); flag != "" {
			flags = append(flags, flag)
		}
	}
	return flags
}

func HasState(state, flag string) bool {
	for _, candidate := range StateFlags(state) {
		if strings.EqualFold(candidate, flag) {
			return true
		}
	}
	return false
}

// IsTerminal reports whether the transfer left the wire.
func IsTerminal(state string) bool {
	return HasState(state, "Completed")
}

// TransferSucceeded reports whether a terminal transfer downloaded fully.
func TransferSucceeded(state string) bool {
	return HasState(state, "Succeeded")
}

// TransferFailed reports whether a terminal transfer ended in a failure category.
func TransferFailed(state string) bool {
	for _, flag := range []string{"Errored", "Rejected", "Aborted", "Cancelled", "TimedOut"} {
		if HasState(state, flag) {
			return true
		}
	}
	return false
}
