package discord

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// serveOnSocket starts a canned-JSON server on a fresh unix socket in a temp dir.
func serveOnSocket(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "test.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		shutdown := server.Close()
		listener.Close()
		if shutdown != nil && !strings.Contains(shutdown.Error(), "closed") {
			t.Errorf("server close: %v", shutdown)
		}
	})
	return socket
}

func TestKatydResolve(t *testing.T) {
	socket := serveOnSocket(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/resolve" {
			t.Errorf("path = %q", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("artist") != "The Beatles" || q.Get("album") != "Abbey Road" || q.Get("limit") != "8" {
			t.Errorf("query = %q", q)
		}
		if q.Get("year") != "" || q.Get("mbid") != "" {
			t.Errorf("empty fields sent: %q", q)
		}
		io.WriteString(w, `{"candidates":[{"release_id":"grp-1","title":"Abbey Road","artist":"The Beatles","date":"1969-09-26","track_count":17,"primary_type":"Album"}],"auto":false}`)
	})
	k := NewKatyd(socket)
	candidates, auto, err := k.Resolve(Query{Artist: "The Beatles", Album: "Abbey Road", Limit: 8})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if auto {
		t.Fatalf("auto = true")
	}
	if len(candidates) != 1 || candidates[0].ReleaseID != "grp-1" || candidates[0].TrackCount != 17 || candidates[0].Date != "1969-09-26" {
		t.Fatalf("candidates = %+v", candidates)
	}
}

func TestKatydResolveSendsYearAndMBID(t *testing.T) {
	socket := serveOnSocket(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("year") != "1969" || q.Get("mbid") != "grp-9" {
			t.Errorf("query = %q", q)
		}
		io.WriteString(w, `{"candidates":[],"auto":false}`)
	})
	k := NewKatyd(socket)
	candidates, _, err := k.Resolve(Query{Artist: "a", Album: "b", Year: 1969, MBID: "grp-9", Limit: 8})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("candidates = %+v", candidates)
	}
}

func TestKatydResolveError(t *testing.T) {
	socket := serveOnSocket(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, `{"error":"katyd is down"}`)
	})
	k := NewKatyd(socket)
	_, _, err := k.Resolve(Query{Artist: "a", Album: "b", Limit: 8})
	if err == nil || !strings.Contains(err.Error(), "katyd is down") {
		t.Fatalf("err = %v, want katyd is down", err)
	}
}

func TestFetchdAddSendsGroup(t *testing.T) {
	socket := serveOnSocket(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/wants" {
			t.Errorf("method/path = %s %q", r.Method, r.URL.Path)
		}
		var body AddWant
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if body.Group != "grp-1" || body.MBID != "" || body.Artist != "The Beatles" || body.Album != "Abbey Road" || body.Year != 1969 {
			t.Errorf("body = %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"want":{"id":"abc123","artist":"The Beatles","album":"Abbey Road","state":"queued"}}`)
	})
	f := NewFetchd(socket)
	want, err := f.Add(AddWant{Artist: "The Beatles", Album: "Abbey Road", Year: 1969, Group: "grp-1"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if want.ID != "abc123" || want.State != stateQueued {
		t.Fatalf("want = %+v", want)
	}
}

func TestFetchdAddSendsMBID(t *testing.T) {
	socket := serveOnSocket(t, func(w http.ResponseWriter, r *http.Request) {
		var body AddWant
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if body.MBID != "grp-9" || body.Group != "" {
			t.Errorf("body = %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"want":{"id":"x","state":"queued"}}`)
	})
	f := NewFetchd(socket)
	if _, err := f.Add(AddWant{Artist: "a", Album: "b", MBID: "grp-9"}); err != nil {
		t.Fatalf("add: %v", err)
	}
}

func TestFetchdWantAndWants(t *testing.T) {
	socket := serveOnSocket(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/want" && r.URL.Query().Get("id") == "abc":
			io.WriteString(w, `{"want":{"id":"abc","state":"needs_decision","error":"boom","album_id":"alb-1","decision_token":"tok","notes":["one","two","three"]}}`)
		case r.URL.Path == "/wants":
			io.WriteString(w, `{"wants":[{"id":"abc","state":"queued"}]}`)
		default:
			t.Errorf("unexpected request %s %q", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	f := NewFetchd(socket)
	want, err := f.Want("abc")
	if err != nil {
		t.Fatalf("want: %v", err)
	}
	if want.State != stateNeedsDecision || want.Error != "boom" || want.AlbumID != "alb-1" || want.DecisionToken != "tok" || len(want.Notes) != 3 {
		t.Fatalf("want = %+v", want)
	}
	wants, err := f.Wants()
	if err != nil {
		t.Fatalf("wants: %v", err)
	}
	if len(wants) != 1 || wants[0].ID != "abc" {
		t.Fatalf("wants = %+v", wants)
	}
}

func TestFetchdWantNotFound(t *testing.T) {
	socket := serveOnSocket(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":"no want with id nope"}`)
	})
	f := NewFetchd(socket)
	if _, err := f.Want("nope"); err == nil || !strings.Contains(err.Error(), "no want with id nope") {
		t.Fatalf("err = %v", err)
	}
}

func TestKatydAlbum(t *testing.T) {
	socket := serveOnSocket(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/album" || r.URL.Query().Get("id") != "Operation Sodasteal/2022 - SLANEY" {
			http.Error(w, `{"error":"no album"}`, http.StatusNotFound)
			return
		}
		io.WriteString(w, `{"id":"x","meta":{"albumartist":"Operation Sodasteal","album":"SLANEY VS. SODASTEAL","release_id":"rel-1"}}`)
	})
	k := NewKatyd(socket)
	album, err := k.Album("Operation Sodasteal/2022 - SLANEY")
	if err != nil {
		t.Fatalf("album: %v", err)
	}
	if album.Meta.AlbumArtist != "Operation Sodasteal" || album.Meta.Album != "SLANEY VS. SODASTEAL" || album.Meta.ReleaseID != "rel-1" {
		t.Fatalf("album = %+v", album)
	}
	if _, err := k.Album("missing"); err == nil {
		t.Fatalf("missing album should fail")
	}
}

type fakeAlbums struct {
	album Album
	err   error
}

func (f fakeAlbums) Album(string) (Album, error) { return f.album, f.err }

func TestImportedIdentityPrefersLibrary(t *testing.T) {
	var imported Album
	imported.Meta.AlbumArtist, imported.Meta.Album, imported.Meta.ReleaseID = "Operation Sodasteal", "SLANEY VS. SODASTEAL", "rel-1"
	typed := Want{Artist: "operation sodastel", Album: "slaney vs sodastel", AlbumID: "dir"}

	artist, album, releaseID := importedIdentity(fakeAlbums{album: imported}, typed)
	if artist != "Operation Sodasteal" || album != "SLANEY VS. SODASTEAL" || releaseID != "rel-1" {
		t.Fatalf("identity = %q %q %q", artist, album, releaseID)
	}

	artist, album, releaseID = importedIdentity(fakeAlbums{err: errors.New("down")}, typed)
	if artist != typed.Artist || album != typed.Album || releaseID != "" {
		t.Fatalf("lookup failure should fall back to typed names: %q %q %q", artist, album, releaseID)
	}
}

func TestDialSocketTimeout(t *testing.T) {
	f := NewFetchd(filepath.Join(t.TempDir(), "missing.sock"))
	start := time.Now()
	if _, err := f.Wants(); err == nil {
		t.Fatalf("expected error on missing socket")
	}
	if elapsed := time.Since(start); elapsed > clientTimeout+2*time.Second {
		t.Fatalf("request took %v, want within client timeout", elapsed)
	}
}
