package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const chatPath = "/v1/chat/completions"

func chatReply(t *testing.T, w http.ResponseWriter, content string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{"message": map[string]any{"content": content}}},
	}); err != nil {
		t.Errorf("write reply: %v", err)
	}
}

func TestChatJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != chatPath {
			t.Errorf("path = %q, want %q", r.URL.Path, chatPath)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k1" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer k1")
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want %q", got, "application/json")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		for _, want := range []string{"json_object", "test-model", "system msg", "user msg", `"role":"system"`, `"role":"user"`} {
			if !strings.Contains(string(body), want) {
				t.Errorf("body missing %q: %s", want, body)
			}
		}
		chatReply(t, w, `{"pick":"a"}`)
	}))
	defer srv.Close()

	c := New(srv.URL+"/v1/", "k1", "test-model")
	var out struct{ Pick string }
	if err := c.ChatJSON(context.Background(), "system msg", "user msg", &out); err != nil {
		t.Fatalf("ChatJSON: %v", err)
	}
	if out.Pick != "a" {
		t.Errorf("Pick = %q, want %q", out.Pick, "a")
	}
}

func TestChatJSONTrailingSlash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != chatPath {
			t.Errorf("path = %q, want %q", r.URL.Path, chatPath)
		}
		chatReply(t, w, `{"pick":"b"}`)
	}))
	defer srv.Close()

	c := New(srv.URL+"/v1", "", "m")
	var out struct{ Pick string }
	if err := c.ChatJSON(context.Background(), "s", "u", &out); err != nil {
		t.Fatalf("ChatJSON: %v", err)
	}
	if out.Pick != "b" {
		t.Errorf("Pick = %q, want %q", out.Pick, "b")
	}
}

func TestChatJSONStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL+"/v1", "", "m")
	err := c.ChatJSON(context.Background(), "s", "u", &struct{}{})
	if err == nil {
		t.Fatal("ChatJSON: nil error, want failure")
	}
	if !strings.Contains(err.Error(), "llm: status 500") {
		t.Errorf("error = %q, want it to contain %q", err, "llm: status 500")
	}
}

func TestChatJSONBadContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatReply(t, w, "not json")
	}))
	defer srv.Close()

	c := New(srv.URL+"/v1", "", "m")
	err := c.ChatJSON(context.Background(), "s", "u", &struct{}{})
	if err == nil {
		t.Fatal("ChatJSON: nil error, want failure")
	}
	if !strings.Contains(err.Error(), "llm: decode") {
		t.Errorf("error = %q, want it to contain %q", err, "llm: decode")
	}
}

func TestEnabled(t *testing.T) {
	if New("", "x", "m").Enabled() {
		t.Error(`New("", "x", "m").Enabled() = true, want false`)
	}
	if !New("http://x", "", "m").Enabled() {
		t.Error(`New("http://x", "", "m").Enabled() = false, want true`)
	}
	if !New("http://x", "k", "m").Enabled() {
		t.Error(`New("http://x", "k", "m").Enabled() = false, want true`)
	}
	var nilc *Client
	if nilc.Enabled() {
		t.Error("nil client Enabled() = true, want false")
	}
}
