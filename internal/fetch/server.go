package fetch

import (
	"encoding/json"
	"net/http"
)

type Server struct {
	Orchestrator *Orchestrator
}

type addRequest struct {
	Artist string `json:"artist"`
	Album  string `json:"album"`
	Year   int    `json:"year,omitempty"`
	MBID   string `json:"mbid,omitempty"`
}

type decideRequest struct {
	ID   string `json:"id"`
	Pick int    `json:"pick"`
	Skip bool   `json:"skip"`
}

type wantResponse struct {
	Want Want `json:"want"`
}

type wantsResponse struct {
	Wants []Want `json:"wants"`
}



func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", s.status)
	mux.HandleFunc("GET /wants", s.wants)
	mux.HandleFunc("POST /wants", s.add)
	mux.HandleFunc("GET /want", s.want)
	mux.HandleFunc("POST /want/decide", s.decide)
	mux.HandleFunc("DELETE /want", s.remove)
	return mux
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	counts := map[string]int{}
	for _, want := range s.Orchestrator.Wants() {
		counts[want.State]++
	}
	writeJSON(w, http.StatusOK, map[string]any{"wants": counts})
}

func (s *Server) wants(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	out := []Want{}
	for _, want := range s.Orchestrator.Wants() {
		if state == "" || want.State == state {
			out = append(out, want)
		}
	}
	writeJSON(w, http.StatusOK, wantsResponse{Wants: out})
}

func (s *Server) want(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id parameter is required")
		return
	}
	found, ok := s.Orchestrator.Want(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no want with id "+id)
		return
	}
	writeJSON(w, http.StatusOK, wantResponse{Want: found})
}

func (s *Server) add(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req addRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode request body: "+err.Error())
		return
	}
	want, err := s.Orchestrator.Add(req.Artist, req.Album, req.Year, req.MBID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, wantResponse{Want: want})
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req decideRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode request body: "+err.Error())
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	want, err := s.Orchestrator.Decide(r.Context(), req.ID, req.Pick, req.Skip)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, wantResponse{Want: want})
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id parameter is required")
		return
	}
	if err := s.Orchestrator.Remove(id); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, map[string]string{"error": message})
}

