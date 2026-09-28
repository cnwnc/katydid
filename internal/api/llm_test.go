package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"doppel.moe/katydid/internal/llm"
)

func llmStub(t *testing.T, content string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": content}},
			},
		})
	}))
}

func postLLM(t *testing.T, handler http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
	handler.ServeHTTP(rec, req)
	return rec
}

func TestLLMPickReturnsListedID(t *testing.T) {
	stub := llmStub(t, `{"pick":"rg-2","note":"standard edition"}`)
	defer stub.Close()
	server := &Server{LLM: llm.New(stub.URL, "k", "m")}
	rec := postLLM(t, server.Handler(), "/llm/pick", llmPickRequest{
		Request: "A / B - Split",
		Options: []llmOption{{ID: "rg-1", Title: "X"}, {ID: "rg-2", Title: "A / B"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out llmPickResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Pick != "rg-2" || out.Note != "standard edition" {
		t.Fatalf("pick %q note %q, want rg-2/standard edition", out.Pick, out.Note)
	}
}

func TestLLMPickInventedIDClearedToNone(t *testing.T) {
	stub := llmStub(t, `{"pick":"not-offered","note":"guess"}`)
	defer stub.Close()
	server := &Server{LLM: llm.New(stub.URL, "k", "m")}
	rec := postLLM(t, server.Handler(), "/llm/pick", llmPickRequest{
		Request: "A - B",
		Options: []llmOption{{ID: "rg-1", Title: "B"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out llmPickResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Pick != "" {
		t.Fatalf("invented id must not pass through: %q", out.Pick)
	}
}

func TestLLMEndpointsNeedConfig(t *testing.T) {
	server := &Server{}
	rec := postLLM(t, server.Handler(), "/llm/pick", llmPickRequest{Options: []llmOption{{ID: "x"}}})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("pick without llm: status %d, want 503", rec.Code)
	}
	rec = postLLM(t, server.Handler(), "/llm/alias", llmAliasRequest{Artist: "a", Album: "b"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("alias without llm: status %d, want 503", rec.Code)
	}
}

func TestLLMAliasDedupesAndCaps(t *testing.T) {
	stub := llmStub(t, `{"variants":[{"artist":"A","album":"B"},{"artist":"A","album":"B"},{"artist":"C","album":"D"},{"artist":"E","album":"F"},{"artist":"G","album":"H"}]}`)
	defer stub.Close()
	server := &Server{LLM: llm.New(stub.URL, "k", "m")}
	rec := postLLM(t, server.Handler(), "/llm/alias", llmAliasRequest{Artist: "a", Album: "b"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out llmAliasResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Variants) != 3 {
		t.Fatalf("variants %d, want capped at 3 deduplicated: %+v", len(out.Variants), out.Variants)
	}
	if out.Variants[0].Artist != "A" || out.Variants[1].Artist != "C" {
		t.Fatalf("dedupe kept the wrong ones: %+v", out.Variants)
	}
}
