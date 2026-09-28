package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// The LLM is optional; when nil or disabled the endpoints answer 503
// and callers fall back to their own handling.

type llmOption struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Artist string `json:"artist"`
	Date   string `json:"date,omitempty"`
	Tracks int    `json:"tracks,omitempty"`
	Type   string `json:"type,omitempty"`
}

type llmPickRequest struct {
	Request string      `json:"request"`
	Options []llmOption `json:"options"`
}

type llmPickResponse struct {
	Pick string `json:"pick"`
	Note string `json:"note,omitempty"`
}

type llmAliasVariant struct {
	Artist string `json:"artist"`
	Album  string `json:"album"`
}

type llmAliasRequest struct {
	Artist string `json:"artist"`
	Album  string `json:"album"`
}

type llmAliasResponse struct {
	Variants []llmAliasVariant `json:"variants"`
}

// pickSystem is the standing instruction for candidate selection: the
// model only ever chooses among MusicBrainz's own candidates and must
// prefer regular editions over deluxe ones.
const pickSystem = `You pick the best matching music release group from a JSON list of MusicBrainz candidates. Answer with one JSON object {"pick": "<id>", "note": "<short reason>"} and nothing else. "pick" must be one of the listed ids, or "" when none matches. Prefer standard albums; do NOT pick deluxe, remastered, expanded, special, or bonus-track editions unless every option is one.`

func (s *Server) llmReady(w http.ResponseWriter) bool {
	if !s.LLM.Enabled() {
		writeError(w, http.StatusServiceUnavailable, "llm is not configured on this daemon")
		return false
	}
	return true
}

func (s *Server) llmPick(w http.ResponseWriter, r *http.Request) {
	if !s.llmReady(w) {
		return
	}
	var req llmPickRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode request: "+err.Error())
		return
	}
	if len(req.Options) == 0 {
		writeError(w, http.StatusBadRequest, "no options to pick from")
		return
	}
	listed := map[string]bool{}
	for _, option := range req.Options {
		listed[option.ID] = true
	}
	options, err := json.Marshal(req.Options)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode options: "+err.Error())
		return
	}
	user := "Request: " + req.Request + "\nOptions: " + string(options)
	var out llmPickResponse
	if err := s.LLM.ChatJSON(r.Context(), pickSystem, user, &out); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if !listed[out.Pick] {
		// the model invented an id: treat as no pick, not as truth
		out.Note = strings.TrimSpace("picked id not among options; " + out.Note)
		out.Pick = ""
	}
	writeJSON(w, http.StatusOK, out)
}

// aliasSystem asks only for respellings; the variants are re-searched
// against MusicBrainz, so invented titles die in the search.
const aliasSystem = `You correct music artist and album name spellings for a MusicBrainz search. Answer with one JSON object {"variants": [{"artist": "...", "album": "..."}]} and nothing else. Give 1 to 3 variants: fixed spellings, transliterations into Latin script, or translated titles. Never invent different albums; only respell or transliterate the given names.`

func (s *Server) llmAlias(w http.ResponseWriter, r *http.Request) {
	if !s.llmReady(w) {
		return
	}
	var req llmAliasRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode request: "+err.Error())
		return
	}
	if req.Artist == "" && req.Album == "" {
		writeError(w, http.StatusBadRequest, "artist and album are empty")
		return
	}
	user := fmt.Sprintf("Artist: %s\nAlbum: %s", req.Artist, req.Album)
	var out llmAliasResponse
	if err := s.LLM.ChatJSON(r.Context(), aliasSystem, user, &out); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	variants := []llmAliasVariant{}
	seen := map[string]bool{}
	for _, v := range out.Variants {
		if v.Artist == "" && v.Album == "" {
			continue
		}
		key := v.Artist + "\x00" + v.Album
		if seen[key] || len(variants) >= 3 {
			continue
		}
		seen[key] = true
		variants = append(variants, v)
	}
	writeJSON(w, http.StatusOK, llmAliasResponse{Variants: variants})
}
