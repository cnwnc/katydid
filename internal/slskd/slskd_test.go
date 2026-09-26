package slskd_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"doppel.moe/katydid/internal/slskd"
)

func newTestServer(t *testing.T, handle func(r *http.Request, body []byte) (any, int)) (*slskd.Client, *[]captured) {
	t.Helper()
	seen := []captured{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, captured{request: *r, body: string(body)})
		out, status := handle(r, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if out != nil {
			_ = json.NewEncoder(w).Encode(out)
		}
	}))
	t.Cleanup(server.Close)
	return slskd.New(server.URL, "test-key-123456"), &seen
}

type captured struct {
	request http.Request
	body    string
}

func TestAuthHeader(t *testing.T) {
	client, seen := newTestServer(t, func(r *http.Request, _ []byte) (any, int) {
		return []slskd.Search{}, http.StatusOK
	})
	if _, err := client.Searches(context.Background()); err != nil {
		t.Fatalf("searches: %v", err)
	}
	if key := (*seen)[0].request.Header.Get("X-API-Key"); key != "test-key-123456" {
		t.Fatalf("X-API-Key = %q", key)
	}
}

func TestCreateSearch(t *testing.T) {
	client, seen := newTestServer(t, func(r *http.Request, _ []byte) (any, int) {
		return slskd.Search{ID: "abc", SearchText: "saetia", State: "InProgress"}, http.StatusOK
	})
	search, err := client.CreateSearch(context.Background(), slskd.SearchRequest{SearchText: "saetia", SearchTimeout: 15})
	if err != nil {
		t.Fatalf("create search: %v", err)
	}
	if search.ID != "abc" {
		t.Fatalf("id = %q", search.ID)
	}
	captured := (*seen)[0]
	if captured.request.URL.Path != "/api/v0/searches" {
		t.Fatalf("path = %q", captured.request.URL.Path)
	}
	var sent slskd.SearchRequest
	if err := json.Unmarshal([]byte(captured.body), &sent); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	if sent.SearchText != "saetia" || sent.SearchTimeout != 15 {
		t.Fatalf("sent = %+v", sent)
	}
}

func TestSearchIncludeResponses(t *testing.T) {
	client, seen := newTestServer(t, func(r *http.Request, _ []byte) (any, int) {
		return slskd.Search{ID: "abc", IsComplete: true, Responses: []slskd.Response{{
			Username: "peer", Files: []slskd.File{{Filename: "SAETIA - 1998/01 some.mp3", Size: 1000}},
		}}}, http.StatusOK
	})
	search, err := client.Search(context.Background(), "abc", true)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !search.IsComplete || len(search.Responses) != 1 {
		t.Fatalf("search = %+v", search)
	}
	if got := (*seen)[0].request.URL.RawQuery; got != "includeResponses=true" {
		t.Fatalf("query = %q", got)
	}
}

func TestEnqueueDownloads(t *testing.T) {
	client, seen := newTestServer(t, func(r *http.Request, _ []byte) (any, int) {
		return nil, http.StatusOK
	})
	files := []slskd.File{{Filename: "dir/01.flac", Size: 5}, {Filename: "dir/02.flac", Size: 6}}
	if err := client.EnqueueDownloads(context.Background(), "peer", files); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	captured := (*seen)[0]
	if captured.request.URL.Path != "/api/v0/transfers/downloads/peer" {
		t.Fatalf("path = %q", captured.request.URL.Path)
	}
	if !strings.HasPrefix(captured.body, "[") {
		t.Fatalf("body must be a JSON array, got %q", captured.body)
	}
	var sent []slskd.QueueDownloadRequest
	if err := json.Unmarshal([]byte(captured.body), &sent); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	if len(sent) != 2 || sent[0].Filename != "dir/01.flac" || sent[0].Size != 5 {
		t.Fatalf("sent = %+v", sent)
	}
}

func TestDownloadsParsing(t *testing.T) {
	client, _ := newTestServer(t, func(r *http.Request, _ []byte) (any, int) {
		return []slskd.UserResponse{{
			Username: "peer",
			Directories: []slskd.DirectoryResponse{{
				Directory: "SAETIA - 1998", FileCount: 1,
				Files: []slskd.Transfer{{
					ID: "t1", Username: "peer", Filename: "SAETIA - 1998/01.flac",
					State: "Completed, Succeeded", BytesTransferred: 5, PercentComplete: 100,
				}},
			}},
		}}, http.StatusOK
	})
	users, err := client.Downloads(context.Background())
	if err != nil {
		t.Fatalf("downloads: %v", err)
	}
	if len(users) != 1 || users[0].Username != "peer" {
		t.Fatalf("users = %+v", users)
	}
	transfer := users[0].Directories[0].Files[0]
	if !slskd.IsTerminal(transfer.State) || !slskd.TransferSucceeded(transfer.State) {
		t.Fatalf("state %q should be terminal+success", transfer.State)
	}
}

func TestErrorCarriesStatus(t *testing.T) {
	client, _ := newTestServer(t, func(r *http.Request, _ []byte) (any, int) {
		return map[string]string{"error": "nope"}, http.StatusUnauthorized
	})
	_, err := client.Searches(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want 401 mention", err)
	}
}

func TestStateFlags(t *testing.T) {
	cases := []struct {
		state                string
		terminal, ok, failed bool
	}{
		{"Completed, Succeeded", true, true, false},
		{"Completed, Errored", true, false, true},
		{"InProgress", false, false, false},
		{"Queued, Remotely", false, false, false},
		{"Requested", false, false, false},
		{"Completed, TimedOut", true, false, true},
		{"Cancelled, Locally", false, false, true},
		{"Completed, Cancelled, Remotely", true, false, true},
	}
	for _, tc := range cases {
		if got := slskd.IsTerminal(tc.state); got != tc.terminal {
			t.Errorf("IsTerminal(%q) = %v", tc.state, got)
		}
		if got := slskd.TransferSucceeded(tc.state); got != tc.ok {
			t.Errorf("TransferSucceeded(%q) = %v", tc.state, got)
		}
		if got := slskd.TransferFailed(tc.state); got != tc.failed {
			t.Errorf("TransferFailed(%q) = %v", tc.state, got)
		}
	}
}

func TestDecodeNaiveTimestamps(t *testing.T) {
	var out []slskd.Transfer
	payload := []byte(`[{"id":"t1","requestedAt":"2026-06-25T01:41:01.7305916","endedAt":"2026-06-25T01:45:01Z"}]`)
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatalf("decode naive timestamps: %v", err)
	}
	if out[0].RequestedAt.IsZero() {
		t.Fatalf("requestedAt should decode")
	}
}

func TestDecodeTransferDurationShapes(t *testing.T) {
	payload := []byte(`[{"id":"t1","remainingTime":"00:00:00","requestedAt":"2026-06-25T01:41:01.7305916","state":"Completed, Succeeded"},{"id":"t2","remainingTime":"01:02:03.5","state":"InProgress"},{"id":"t3","remainingTime":null}]`)
	var out []slskd.Transfer
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatalf("decode transfers: %v", err)
	}
	if out[0].RemainingTime == nil || out[0].RemainingTime.Duration != 0 {
		t.Errorf("zero timespan: %+v", out[0].RemainingTime)
	}
	if out[1].RemainingTime == nil || out[1].RemainingTime.Duration != time.Hour+2*time.Minute+3*time.Second+500*time.Millisecond {
		t.Errorf("fractional timespan: %+v", out[1].RemainingTime)
	}
	if out[2].RemainingTime != nil {
		t.Errorf("null should stay nil: %+v", out[2].RemainingTime)
	}
}
