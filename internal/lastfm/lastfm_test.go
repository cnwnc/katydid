package lastfm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// serve returns a client pointed at a one-response test server plus a
// func exposing the last request that server received.
func serve(t *testing.T, status int, body string) (*Client, func() *http.Request) {
	t.Helper()
	var mu sync.Mutex
	var last *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		last = r
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return newWithBase(srv.URL, "test-key"), func() *http.Request {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

func TestGetInfoFull(t *testing.T) {
	const body = `{"album":{` +
		`"name":"Mellow",` +
		`"artist":"The Brown",` +
		`"mbid":"0383dadf-2a4e-4d10-a46a-e9e041da8eb3",` +
		`"url":"https://www.last.fm/music/The+Brown/Mellow",` +
		`"listeners":"4242",` +
		`"tracks":{"track":[` +
		`{"name":"One","duration":"213","@attr":{"rank":"1"}},` +
		`{"name":"Two","duration":"","@attr":{"rank":"2"}},` +
		`{"name":"Three","duration":"null","@attr":{"rank":"3"}}` +
		`]}}}`
	c, last := serve(t, http.StatusOK, body)
	got, err := c.GetInfo(context.Background(), "The Brown", "Mellow")
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	want := Album{
		Name:      "Mellow",
		Artist:    "The Brown",
		MBID:      "0383dadf-2a4e-4d10-a46a-e9e041da8eb3",
		URL:       "https://www.last.fm/music/The+Brown/Mellow",
		Listeners: 4242,
		Tracks: []Track{
			{Name: "One", Duration: 213, Rank: 1},
			{Name: "Two", Duration: 0, Rank: 2},
			{Name: "Three", Duration: 0, Rank: 3},
		},
	}
	if got.Name != want.Name || got.Artist != want.Artist || got.MBID != want.MBID ||
		got.URL != want.URL || got.Listeners != want.Listeners {
		t.Fatalf("album = %+v, want %+v", got, want)
	}
	if len(got.Tracks) != len(want.Tracks) {
		t.Fatalf("tracks = %d, want %d", len(got.Tracks), len(want.Tracks))
	}
	for i, w := range want.Tracks {
		if got.Tracks[i] != w {
			t.Errorf("track %d = %+v, want %+v", i, got.Tracks[i], w)
		}
	}
	q := last().URL.Query()
	for k, v := range map[string]string{
		"method":  "album.getinfo",
		"artist":  "The Brown",
		"album":   "Mellow",
		"api_key": "test-key",
		"format":  "json",
	} {
		if q.Get(k) != v {
			t.Errorf("param %s = %q, want %q", k, q.Get(k), v)
		}
	}
}

func TestGetInfoStub(t *testing.T) {
	const body = `{"album":{"name":"Mellow","artist":"The Brown",` +
		`"mbid":"","url":"https://www.last.fm/music/The+Brown/Mellow","listeners":"7"}}`
	c, _ := serve(t, http.StatusOK, body)
	got, err := c.GetInfo(context.Background(), "The Brown", "Mellow")
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if got.Listeners != 7 {
		t.Errorf("listeners = %d, want 7", got.Listeners)
	}
	if got.Tracks != nil {
		t.Fatalf("tracks = %#v, want nil on stub pages", got.Tracks)
	}
}

func TestGetInfoNotFound(t *testing.T) {
	c, _ := serve(t, http.StatusOK, `{"error":6,"message":"Album not found"}`)
	got, err := c.GetInfo(context.Background(), "Nope", "Nothing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if got.Name != "" || got.Artist != "" || got.MBID != "" || got.URL != "" ||
		got.Listeners != 0 || got.Tracks != nil {
		t.Fatalf("album = %+v, want zero", got)
	}
}

func TestGetInfoOtherError(t *testing.T) {
	c, _ := serve(t, http.StatusOK, `{"error":29,"message":"Rate limit exceeded"}`)
	_, err := c.GetInfo(context.Background(), "The Brown", "Mellow")
	if err == nil {
		t.Fatal("want error")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, must not be ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "Rate limit exceeded") {
		t.Errorf("err = %v, want api message included", err)
	}
}

func TestGetInfoHTTPStatus(t *testing.T) {
	c, _ := serve(t, http.StatusServiceUnavailable, "nope")
	_, err := c.GetInfo(context.Background(), "The Brown", "Mellow")
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v, want 503 status", err)
	}
}

func TestSearchRanking(t *testing.T) {
	const body = `{"results":{"albummatches":{"album":[` +
		`{"name":"Colorix","artist":"Brown Mellow Noise","url":"https://x/1","mbid":"mbid-1"},` +
		`{"name":"MelloW","artist":"The Brown","url":"https://x/2","mbid":"mbid-2"},` +
		`{"name":"Mellow","artist":"Maria Mena","url":"https://x/3","mbid":""}` +
		`]}}}`
	c, last := serve(t, http.StatusOK, body)
	got, err := c.Search(context.Background(), "the brown", "mellow")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("hits = %d, want 3", len(got))
	}
	if got[0].Name != "MelloW" || got[0].Artist != "The Brown" ||
		got[0].URL != "https://x/2" || got[0].MBID != "mbid-2" {
		t.Fatalf("first hit = %+v, want exact MelloW/The Brown", got[0])
	}
	for _, hit := range got[1:] {
		if hit.Artist == "The Brown" && hit.Name == "MelloW" {
			t.Errorf("exact match ranked %v, want first", got)
		}
		if hit.Artist != "Brown Mellow Noise" && hit.Artist != "Maria Mena" {
			t.Errorf("unexpected hit %+v", hit)
		}
	}
	for _, hit := range got {
		if hit.Listeners != 0 {
			t.Errorf("hit %q has listeners %d, want 0 (album.search has none)", hit.Name, hit.Listeners)
		}
	}
	q := last().URL.Query()
	for k, v := range map[string]string{
		"method": "album.search",
		"artist": "the brown",
		"album":  "mellow",
	} {
		if q.Get(k) != v {
			t.Errorf("param %s = %q, want %q", k, q.Get(k), v)
		}
	}
}

func TestSearchPercentEncoding(t *testing.T) {
	const artist = "მგზავრი" // non-ascii (georgian) must survive url encoding
	c, last := serve(t, http.StatusOK, `{"results":{"albummatches":{"album":[]}}}`)
	if _, err := c.Search(context.Background(), artist, "album"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := last().URL.Query().Get("artist"); got != artist {
		t.Fatalf("server saw artist %q, want %q", got, artist)
	}
}

func TestGetInfoContextCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // hang until the client gives up
	}))
	t.Cleanup(srv.Close)
	c := newWithBase(srv.URL, "test-key")
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := c.GetInfo(ctx, "The Brown", "Mellow")
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request ignored context cancellation")
	}
}
