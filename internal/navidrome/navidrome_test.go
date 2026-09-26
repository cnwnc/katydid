package navidrome

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func startNavidrome(t *testing.T, scanning, denyScan *atomic.Bool, expectPatch bool) *Client {
	t.Helper()
	var statusPolls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/login", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["username"] != "katydid" || in["password"] != "secret" {
			http.Error(w, "wrong credentials", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"id":"u1","token":"jwt-token"}`)
	})
	mux.HandleFunc("/rest/startScan", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if denyScan.Load() || r.URL.Query().Get("p") != "secret" {
			fmt.Fprint(w, `{"subsonic-response":{"status":"failed","error":{"code":50,"message":"User is not authorized"}}}`)
			return
		}
		scanning.Store(true)
		fmt.Fprint(w, `{"subsonic-response":{"status":"ok","scanStatus":{"scanning":true}}}`)
	})
	mux.HandleFunc("/rest/getScanStatus", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// a started scan finishes after the second status poll
		if scanning.Load() && statusPolls.Add(1) > 2 {
			scanning.Store(false)
		}
		fmt.Fprintf(w, `{"subsonic-response":{"status":"ok","scanStatus":{"scanning":%v}}}`, scanning.Load())
	})
	mux.HandleFunc("/rest/search3", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"subsonic-response":{"status":"ok","searchResult3":{"album":[`+
			`{"id":"alb-wrong","name":"Other Album","artist":"Someone"},`+
			`{"id":"alb-ok","name":"Test Album","artist":"Test Artist"}]}}}`)
	})
	mux.HandleFunc("/rest/createShare", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if id := r.URL.Query().Get("id"); id != "alb-ok" {
			fmt.Fprint(w, `{"subsonic-response":{"status":"failed","error":{"message":"no such album"}}}`)
			return
		}
		fmt.Fprint(w, `{"subsonic-response":{"status":"ok","shares":{"share":[{"id":"sh1","url":"`+serverURL(r)+`/share/sh1"}]}}}`)
	})
	patched := false
	mux.HandleFunc("/api/share/sh1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("x-nd-authorization") != "Bearer jwt-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var in map[string]bool
		_ = json.NewDecoder(r.Body).Decode(&in)
		if !in["downloadable"] {
			http.Error(w, "downloadable not set", http.StatusBadRequest)
			return
		}
		patched = true
		scanning.Store(false)
		fmt.Fprint(w, `{"id":"sh1","downloadable":true}`)
	})
	t.Cleanup(func() {
		if expectPatch && !patched {
			t.Errorf("downloadable flag never set")
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return New(server.URL, "katydid", "secret")
}

func serverURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func TestRefreshAndShare(t *testing.T) {
	var scanning, denyScan atomic.Bool
	client := startNavidrome(t, &scanning, &denyScan, true)

	url, err := client.RefreshAndShare(context.Background(), "Test Artist", "Test Album")
	if err != nil {
		t.Fatalf("RefreshAndShare: %v", err)
	}
	if !strings.HasSuffix(url, "/share/sh1") {
		t.Errorf("share url: %q", url)
	}
}

func TestRefreshScanDeniedProceedsWhenIdle(t *testing.T) {
	var scanning, denyScan atomic.Bool
	denyScan.Store(true)
	client := startNavidrome(t, &scanning, &denyScan, true)

	// no scan rights and nothing scanning: refresh must not block, the
	// share still goes through on the library as it stands
	url, err := client.RefreshAndShare(context.Background(), "Test Artist", "Test Album")
	if err != nil {
		t.Fatalf("RefreshAndShare with denied scan: %v", err)
	}
	if !strings.HasSuffix(url, "/share/sh1") {
		t.Errorf("share url: %q", url)
	}
}

func TestRefreshWaitsForForeignScan(t *testing.T) {
	var scanning, denyScan atomic.Bool
	denyScan.Store(true)
	scanning.Store(true)
	client := startNavidrome(t, &scanning, &denyScan, true)

	// someone else's scan is running: refresh waits for it to end
	if _, err := client.RefreshAndShare(context.Background(), "Test Artist", "Test Album"); err != nil {
		t.Fatalf("RefreshAndShare during foreign scan: %v", err)
	}
}

func TestFindAlbumFailsLoud(t *testing.T) {
	var scanning, denyScan atomic.Bool
	denyScan.Store(true)
	client := startNavidrome(t, &scanning, &denyScan, false)

	if _, err := client.RefreshAndShare(context.Background(), "Other Artist", "Other Album"); err == nil {
		t.Fatalf("missing album should fail loud")
	}
}

func TestBaseURL(t *testing.T) {
	cases := []struct {
		base, port, want string
	}{
		{"", "", "http://127.0.0.1:4533"},
		{"", "4600", "http://127.0.0.1:4600"},
		{"http://localhost", "", "http://localhost:4533"},
		{"http://10.100.0.2", "", "http://10.100.0.2:4533"},
		{"http://10.100.0.2:1234", "", "http://10.100.0.2:1234"},
		{"http://10.100.0.2", "80", "http://10.100.0.2:80"},
		{"http://10.100.0.2:1234/", "", "http://10.100.0.2:1234"},
	}
	for _, c := range cases {
		got, err := BaseURL(c.base, c.port)
		if err != nil {
			t.Fatalf("BaseURL(%q, %q): %v", c.base, c.port, err)
		}
		if got != c.want {
			t.Errorf("BaseURL(%q, %q) = %q, want %q", c.base, c.port, got, c.want)
		}
	}
	if _, err := BaseURL("not-a-url", ""); err == nil {
		t.Errorf("base without scheme should fail loud")
	}
}

func TestLoose(t *testing.T) {
	if loose("SLANEY VS. SODASTEAL") != "slaneyvssodasteal" {
		t.Errorf("loose album: %q", loose("SLANEY VS. SODASTEAL"))
	}
	if loose("Operation Sodasteal") != "operationsodasteal" {
		t.Errorf("loose artist: %q", loose("Operation Sodasteal"))
	}
}
