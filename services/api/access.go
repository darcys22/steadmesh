package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/darcys22/steadmesh/pkg/access"
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/credentials"
)

// access returns the seat's current sandbox access from its committed
// manifest. The egress gateway authorises every connection with it and the
// seat runner polls it, so live changes (hosts, revocation) apply within
// seconds and a retired seat loses access at once.
func (s *server) access(w http.ResponseWriter, r *http.Request, q *seatReq) {
	a := q.seat.Manifest.Access
	out := runtimeapi.AccessResponse{SeatID: q.seat.ID, SeatKey: q.seat.Key, Egress: []access.EgressRule{}}
	if a != nil {
		out.Egress = append(out.Egress, a.Egress...)
		out.Browser = a.Browser != nil
		if a.Browser != nil {
			out.BrowserSession = a.Browser.SessionConnection
		}
		for _, g := range a.GitHub {
			if g.Delivery == access.DeliverySandbox {
				out.GitHub = append(out.GitHub, runtimeapi.GitHubSandbox{Connection: g.Connection, Host: g.Host})
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// credential delivers a credential of a connection into the seat's sandbox
// when its access grants sandbox delivery (git and gh, the browser). Every
// issue and refusal is recorded on the seat's execution; the token itself
// never is.
func (s *server) credential(w http.ResponseWriter, r *http.Request, q *seatReq) {
	conn := r.PathValue("connection")
	annotate(r.Context(), "connection", conn)
	if s.Credentials == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "credential delivery is not configured")
		return
	}
	c, err := s.Credentials.Issue(r.Context(), q.seat.OrganizationID, q.seat.Key, q.seat.Manifest.Access, conn)
	if errors.Is(err, credentials.ErrForbidden) {
		s.accessEvent(r, q, runtimeapi.EventCredentialDenied, map[string]any{"connection": conn, "reason": err.Error()})
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	if err != nil {
		s.accessEvent(r, q, runtimeapi.EventCredentialDenied, map[string]any{"connection": conn, "reason": err.Error()})
		writeError(w, http.StatusBadGateway, "unavailable", "could not issue a credential: "+err.Error())
		return
	}
	data := map[string]any{"connection": conn, "revocable": c.Revocable}
	if !c.ExpiresAt.IsZero() {
		data["expires_at"] = c.ExpiresAt
	}
	if g := q.seat.Manifest.Access.GitHubFor(conn); g != nil {
		data["repos"], data["permissions"] = g.Repos, g.Permissions
	}
	s.accessEvent(r, q, runtimeapi.EventCredentialIssued, data)
	writeJSON(w, http.StatusOK, c)
}

// accessEvent records a sandbox access event on the seat's execution.
func (s *server) accessEvent(r *http.Request, q *seatReq, kind string, data map[string]any) {
	f, ok := q.fence()
	if q.execution == "" || !ok {
		return
	}
	b, _ := json.Marshal(data)
	ev := runtimeapi.ExecutionEvent{Kind: kind, Time: time.Now().UTC(), Data: b}
	if err := s.Store.AppendEvents(context.WithoutCancel(r.Context()), f, q.execution, []runtimeapi.ExecutionEvent{ev}); err != nil {
		annotate(r.Context(), "access_event_error", err.Error())
	}
}

// reconcileCredentials revokes delivered credentials whose grant the new
// manifest removed. Retiring seats keep their access until they retire
// (services/retirement revokes it then).
func (s *server) reconcileCredentials(r *http.Request, orgID string, m *compile.Manifest) {
	if s.Credentials == nil {
		return
	}
	seats := map[string]*access.SeatAccess{}
	retiring, err := s.Store.RetiringManifests(r.Context(), orgID)
	if err != nil {
		s.Log.Warn("list retiring seats; their credentials are reconciled on the next sync", "err", err)
		return
	}
	for k, sm := range retiring {
		seats[k] = sm.Access
	}
	for k, sm := range m.Seats {
		seats[k] = sm.Access
	}
	for _, rv := range s.Credentials.Reconcile(context.WithoutCancel(r.Context()), orgID, seats) {
		if rv.Err != nil {
			s.Log.Warn("credential not revoked; it expires on its own", "seat", rv.Seat, "connection", rv.Connection, "err", rv.Err)
		}
	}
}
