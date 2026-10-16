package api

import (
	"net/http"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

func (s *server) createElasticTeam(w http.ResponseWriter, r *http.Request, q *seatReq) {
	f, ok := q.fence()
	if !ok {
		fenced(w)
		return
	}
	var req runtimeapi.ElasticTeamRequest
	if !decodeBody(w, r, &req) {
		return
	}
	team, err := s.Store.CreateElasticTeam(r.Context(), f, q.execution, req)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, team)
}
func (s *server) getElasticTeam(w http.ResponseWriter, r *http.Request, q *seatReq) {
	team, err := s.Store.GetElasticTeam(r.Context(), q.seat.ID, r.PathValue("team"))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, team)
}
func (s *server) cancelElasticTeam(w http.ResponseWriter, r *http.Request, q *seatReq) {
	f, ok := q.fence()
	if !ok {
		fenced(w)
		return
	}
	if err := s.Store.CancelElasticTeam(r.Context(), f, r.PathValue("team")); err != nil {
		s.storeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
