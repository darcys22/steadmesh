package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/store"
)

// sync establishes identities and activates the new policy revision before
// returning, then rebuilds the organisation's connection adapters.
func (s *server) sync(w http.ResponseWriter, r *http.Request) {
	var req runtimeapi.SyncRequest
	if !decodeBody(w, r, &req) {
		return
	}
	var m compile.Manifest
	if err := json.Unmarshal(req.Manifest, &m); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", "manifest: "+err.Error())
		return
	}
	if req.Namespace == "" || req.Key == "" || m.Spec.Key != req.Key {
		writeError(w, http.StatusBadRequest, "invalid", "namespace and key are required and key must match the manifest")
		return
	}
	for k, sm := range m.Seats {
		if sm.Key != k {
			writeError(w, http.StatusBadRequest, "invalid", fmt.Sprintf("manifest seat %q has key %q", k, sm.Key))
			return
		}
	}
	res, err := s.Store.SyncOrganization(r.Context(), store.SyncInput{Namespace: req.Namespace, Key: req.Key, SourceUID: req.SourceUID, Manifest: &m})
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	annotate(r.Context(), "organization_id", res.OrganizationID, "revision", m.Digest, "retired", res.Retired)
	s.Connections.Sync(r.Context(), res.OrganizationID, m.Spec.Connections)
	writeJSON(w, http.StatusOK, res)
}

func (s *server) org(w http.ResponseWriter, r *http.Request) (*store.Organization, bool) {
	org, err := s.Store.Organization(r.Context(), r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, err)
		return nil, false
	}
	annotate(r.Context(), "organization_id", org.ID)
	return org, true
}

func (s *server) runtime(w http.ResponseWriter, r *http.Request) {
	org, ok := s.org(w, r)
	if !ok {
		return
	}
	seats, err := s.Store.SeatRuntimes(r.Context(), org.ID, nil)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, runtimeapi.RuntimeResponse{Seats: seats})
}

func (s *server) verify(w http.ResponseWriter, r *http.Request) {
	org, ok := s.org(w, r)
	if !ok {
		return
	}
	res := s.Connections.Verify(r.Context(), org.ID, &org.Manifest)
	for key, c := range res.Connections {
		if err := s.Store.RecordConnectionCheck(r.Context(), org.ID, key, c); err != nil {
			s.storeError(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, res)
}

// deleteOrg removes the organisation's runtime. ?retention=retain|delete
// overrides the declared data retention.
func (s *server) deleteOrg(w http.ResponseWriter, r *http.Request) {
	org, ok := s.org(w, r)
	if !ok {
		return
	}
	retention := r.URL.Query().Get("retention")
	if retention == "" {
		retention = org.Manifest.Spec.DataRetention
	}
	if retention != "retain" && retention != "delete" && retention != "" {
		writeError(w, http.StatusBadRequest, "invalid", "retention must be retain or delete")
		return
	}
	s.Connections.Remove(org.ID)
	if err := s.Store.DeleteOrganization(r.Context(), org.ID, retention == "delete"); err != nil {
		s.storeError(w, r, err)
		return
	}
	annotate(r.Context(), "retention", retention)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) seatOrg(w http.ResponseWriter, r *http.Request) (string, bool) {
	org, err := s.Store.SeatOrganization(r.Context(), r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, err)
		return "", false
	}
	annotate(r.Context(), "organization_id", org, "seat_id", r.PathValue("id"))
	return org, true
}

func (s *server) fence(w http.ResponseWriter, r *http.Request) {
	var req runtimeapi.FenceRequest
	if !decodeBody(w, r, &req) {
		return
	}
	org, ok := s.seatOrg(w, r)
	if !ok {
		return
	}
	l, err := s.Store.FenceSeat(r.Context(), org, r.PathValue("id"), req.ExpectedGeneration)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	s.Log.InfoContext(r.Context(), "seat fenced", "seat_id", l.SeatID, "generation", l.Generation)
	writeJSON(w, http.StatusOK, l)
}

func (s *server) createProbe(w http.ResponseWriter, r *http.Request) {
	org, ok := s.seatOrg(w, r)
	if !ok {
		return
	}
	id, err := s.Store.CreateProbe(r.Context(), org, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	annotate(r.Context(), "message_id", id)
	writeJSON(w, http.StatusAccepted, runtimeapi.ProbeResponse{ProbeID: id, Status: "pending"})
}

func (s *server) getProbe(w http.ResponseWriter, r *http.Request) {
	org, ok := s.seatOrg(w, r)
	if !ok {
		return
	}
	p, err := s.Store.ProbeStatus(r.Context(), org, r.PathValue("id"), r.PathValue("probe"))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}
