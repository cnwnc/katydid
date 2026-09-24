package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"doppel.moe/katydid/internal/importer"
	"doppel.moe/katydid/internal/library"
)

type Server struct {
	Index  *library.Index
	Import *importer.Manager
}

type StatusResponse struct {
	library.Status
}

type AlbumsResponse struct {
	Albums []library.Album `json:"albums"`
}

type CheckResponse struct {
	Findings []library.Finding `json:"findings"`
}

type ScanResponse struct {
	Status library.Status `json:"status"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", s.status)
	mux.HandleFunc("GET /albums", s.albums)
	mux.HandleFunc("GET /album", s.album)
	mux.HandleFunc("POST /scan", s.scan)
	mux.HandleFunc("GET /check", s.check)
	mux.HandleFunc("POST /import", s.importStart)
	mux.HandleFunc("POST /import/decide", s.importDecide)
	mux.HandleFunc("GET /decisions", s.decisions)
	return mux
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, StatusResponse{Status: s.Index.Status()})
}

func (s *Server) albums(w http.ResponseWriter, r *http.Request) {
	query := library.Query{
		Q:      r.URL.Query().Get("q"),
		Artist: r.URL.Query().Get("artist"),
	}
	if year := r.URL.Query().Get("year"); year != "" {
		parsed, err := strconv.Atoi(year)
		if err != nil {
			writeError(w, http.StatusBadRequest, "year must be an integer")
			return
		}
		if parsed < 0 {
			writeError(w, http.StatusBadRequest, "year must be a positive integer")
			return
		}
		query.Year = parsed
	}
	writeJSON(w, http.StatusOK, AlbumsResponse{Albums: s.Index.Albums(query)})
}

func (s *Server) album(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id parameter is required")
		return
	}
	album, err := s.Index.Album(id)
	if err != nil {
		if errors.Is(err, library.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no album with id "+id)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, album)
}

func (s *Server) scan(w http.ResponseWriter, r *http.Request) {
	if err := s.Index.Scan(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ScanResponse{Status: s.Index.Status()})
}

func (s *Server) check(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, CheckResponse{Findings: s.Index.Check()})
}

func (s *Server) importStart(w http.ResponseWriter, r *http.Request) {
	if s.Import == nil {
		writeError(w, http.StatusServiceUnavailable, "import is not configured on this daemon")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req importer.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode request body: "+err.Error())
		return
	}
	if req.Dir == "" {
		writeError(w, http.StatusBadRequest, "dir is required")
		return
	}
	result, err := s.Import.Import(r.Context(), req)
	if err != nil {
		writeImportError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) importDecide(w http.ResponseWriter, r *http.Request) {
	if s.Import == nil {
		writeError(w, http.StatusServiceUnavailable, "import is not configured on this daemon")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		Token string `json:"token"`
		Pick  int    `json:"pick"`
		Skip  bool   `json:"skip"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode request body: "+err.Error())
		return
	}
	if req.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	result, err := s.Import.Decide(r.Context(), req.Token, req.Pick, req.Skip)
	if err != nil {
		writeImportError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) decisions(w http.ResponseWriter, r *http.Request) {
	if s.Import == nil {
		writeJSON(w, http.StatusOK, struct{ Decisions []importer.Decision }{nil})
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Decisions []importer.Decision `json:"decisions"`
	}{s.Import.Decisions()})
}

func writeImportError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, importer.ErrTargetExists):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, importer.ErrNoDecision):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		return
	}
}

func writeError(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, ErrorResponse{Error: message})
}
