package mb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type requests struct {
	URIs []string
}

func fixtureServer(t *testing.T) (*httptest.Server, *requests) {
	t.Helper()
	seen := &requests{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		seen.URIs = append(seen.URIs, r.URL.RequestURI())
		name := "unknown"
		if r.URL.Path == "/ws/2/release" {
			q := r.URL.Query().Get("query")
			switch {
			case contains(q, "Drukqs"):
				name = "search-drukqs.json"
			case contains(q, "No Absolutes"):
				name = "search-gaza.json"
			case contains(q, "Saetia"):
				name = "search-saetia.json"
			}
		} else if id, ok := cutPrefix(r.URL.Path, "/ws/2/release/"); ok {
			switch id {
			case "a3a96dde-8af3-3622-a936-4ac3af501e1d":
				name = "release-drukqs.json"
			case "d021d222-c3e9-44ae-a202-39bb78d6ca0f":
				name = "release-gaza.json"
			case "a18a94c5-03e2-421f-859c-97f076a7674d":
				name = "release-saetia.json"
			}
		}
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Errorf("no fixture for %s: %v", r.URL.RequestURI(), err)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, seen
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return "", false
}

func newTestClient(serverURL, cacheDir string) *Client {
	client := New(serverURL, cacheDir, false)
	client.SetRequestInterval(5 * time.Millisecond)
	return client
}

func TestSearchReleasesParsesFixture(t *testing.T) {
	server, _ := fixtureServer(t)
	client := newTestClient(server.URL, "")

	releases, err := client.SearchReleases(context.Background(), BuildQuery("Aphex Twin", "Drukqs"))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(releases) < 5 {
		t.Fatalf("releases: got %d, want >= 5", len(releases))
	}
	if releases[0].Title != "Drukqs" || releases[0].Artist() != "Aphex Twin" {
		t.Errorf("first release: %s / %s", releases[0].Artist(), releases[0].Title)
	}
	if releases[0].ReleaseGroup == nil || releases[0].ReleaseGroup.ID == "" {
		t.Errorf("release group missing on first release")
	}
}

func TestLookupReleaseParsesFixture(t *testing.T) {
	server, _ := fixtureServer(t)
	client := newTestClient(server.URL, "")

	release, err := client.LookupRelease(context.Background(), "a3a96dde-8af3-3622-a936-4ac3af501e1d")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if release.Title != "Drukqs" || release.Artist() != "Aphex Twin" {
		t.Errorf("release: %s / %s", release.Artist(), release.Title)
	}
	tracks := release.FlattenedTracks()
	if len(tracks) != 30 {
		t.Fatalf("flattened tracks: got %d, want 30", len(tracks))
	}
	if tracks[0].Title != "Jynweythek" {
		t.Errorf("track 1: got %q, want Jynweythek", tracks[0].Title)
	}
	if tracks[17].Title != "Lornaderek" {
		t.Errorf("track 18: got %q, want Lornaderek", tracks[17].Title)
	}
	if release.Media[0].Position == release.Media[1].Position {
		t.Errorf("media positions not distinct: %d, %d", release.Media[0].Position, release.Media[1].Position)
	}
}

func TestCacheHits(t *testing.T) {
	server, seen := fixtureServer(t)
	cacheDir := t.TempDir()
	client := newTestClient(server.URL, cacheDir)

	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := client.SearchReleases(ctx, BuildQuery("Saetia", "Saetia")); err != nil {
			t.Fatalf("search %d: %v", i, err)
		}
	}
	if len(seen.URIs) != 1 {
		t.Errorf("requests: got %d, want 1 (second served from cache)", len(seen.URIs))
	}

	noCacheClient := newTestClient(server.URL, cacheDir)
	noCacheClient.noCache = true
	if _, err := noCacheClient.SearchReleases(ctx, BuildQuery("Saetia", "Saetia")); err != nil {
		t.Fatalf("no-cache search: %v", err)
	}
	if len(seen.URIs) != 2 {
		t.Errorf("requests after no-cache: got %d, want 2", len(seen.URIs))
	}
}

func TestThrottle(t *testing.T) {
	server, _ := fixtureServer(t)
	client := New(server.URL, "", false)
	client.SetRequestInterval(50 * time.Millisecond)

	start := time.Now()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := client.SearchReleases(ctx, BuildQuery("Saetia", "Saetia")); err != nil {
			t.Fatalf("search %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	if elapsed < 100*time.Millisecond {
		t.Errorf("3 requests took %v, want >= 100ms at 50ms interval", elapsed)
	}
}

func TestHTTPErrorFailsLoud(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("busy"))
	}))
	t.Cleanup(server.Close)
	client := newTestClient(server.URL, "")

	_, err := client.SearchReleases(context.Background(), "artist:\"x\"")
	if err == nil {
		t.Fatalf("search against 503: got nil error, want failure")
	}
}

func TestRetryRecoversFrom503(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("overloaded"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"releases": []}`))
	}))
	t.Cleanup(server.Close)
	client := newTestClient(server.URL, "")

	releases, err := client.SearchReleases(context.Background(), "artist:\"x\"")
	if err != nil {
		t.Fatalf("search after 503s: %v", err)
	}
	if len(releases) != 0 {
		t.Errorf("releases: %+v", releases)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (two 503s then success)", calls)
	}
}

func TestRetryGivesUpAfterMax(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(server.URL, "")
	client.SetMaxRetries(2)

	_, err := client.SearchReleases(context.Background(), "artist:\"x\"")
	if err == nil {
		t.Fatalf("persistent 502: got nil error, want failure")
	}
	if want := 3; calls != want {
		t.Errorf("calls = %d, want %d (first attempt plus retries)", calls, want)
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error should mention the status: %v", err)
	}
}

func TestRetryHonorsRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(server.URL, "")
	client.SetMaxRetries(1)

	start := time.Now()
	_, err := client.SearchReleases(context.Background(), "artist:\"x\"")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("persistent 503: got nil error, want failure")
	}
	// The computed backoff (5ms) is smaller than Retry-After (1s), so the
	// wait must stretch to at least the Retry-After floor.
	if elapsed < time.Second {
		t.Errorf("elapsed %v < 1s; Retry-After was not honored", elapsed)
	}
}

