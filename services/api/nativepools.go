package api

import (
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"net/http"
	"strconv"
)

func (s *server) registerNativePools(seat func(string, seatHandler)) {
	seat("GET /v1/teams/{team}/participants", s.listPoolMembers)
	seat("POST /v1/teams/{team}/participants", s.spawnPoolMember)
	seat("POST /v1/teams/{team}/participants/{member}/drain", s.drainPoolMember)
}
func (s *server) listPoolMembers(w http.ResponseWriter, r *http.Request, q *seatReq) {
	out, err := s.Store.ListPoolMembers(r.Context(), q.seat.ID, r.PathValue("team"))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	offset, limit := 0, 20
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid", "cursor must be nonnegative")
			return
		}
		offset = n
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 1 || n > 50 {
			writeError(w, http.StatusBadRequest, "invalid", "limit must be 1..50")
			return
		}
		limit = n
	}
	if offset > len(out) {
		writeError(w, http.StatusBadRequest, "invalid", "cursor beyond participants")
		return
	}
	end := min(offset+limit, len(out))
	next := ""
	if end < len(out) {
		next = strconv.Itoa(end)
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": out[offset:end], "next_cursor": next})
}
func (s *server) spawnPoolMember(w http.ResponseWriter, r *http.Request, q *seatReq) {
	f, ok := q.fence()
	if !ok {
		fenced(w)
		return
	}
	var req runtimeapi.PoolSpawnRequest
	if !decodeBody(w, r, &req) {
		return
	}
	out, err := s.Store.SpawnPoolMember(r.Context(), f, r.PathValue("team"), req.Template, req.RequestID)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}
func (s *server) drainPoolMember(w http.ResponseWriter, r *http.Request, q *seatReq) {
	f, ok := q.fence()
	if !ok {
		fenced(w)
		return
	}
	if err := s.Store.DrainPoolMember(r.Context(), f, r.PathValue("team"), r.PathValue("member")); err != nil {
		s.storeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
