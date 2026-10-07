package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/connectors/model"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/auth"
	"github.com/darcys22/steadmesh/services/connections"
	"github.com/darcys22/steadmesh/services/policy"
	"github.com/darcys22/steadmesh/services/store"
	"github.com/darcys22/steadmesh/services/tools"
)

// seatReq is an authenticated seat request.
type seatReq struct {
	principal auth.Seat
	seat      *store.Seat
	org       *store.Organization
	gen       int64
	hasGen    bool
	execution string
}

// fence returns the request's fence, or false if the generation header is
// missing (which is treated as stale: contracts.md "Seat authentication").
func (q *seatReq) fence() (store.Fence, bool) {
	return store.Fence{SeatID: q.seat.ID, Generation: q.gen}, q.hasGen
}

type seatHandler func(w http.ResponseWriter, r *http.Request, q *seatReq)

// seatAuth authenticates a seat token and loads the seat's committed
// manifest. The model proxy also accepts the token as x-api-key, which is
// how Anthropic-compatible clients send it.
func (s *server) seatAuth(next seatHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		if token == "" && strings.HasPrefix(r.URL.Path, runtimeapi.PathModelProxy) {
			token = r.Header.Get("X-Api-Key")
		}
		p, err := s.Auth.Seat(r.Context(), token)
		if err != nil {
			authError(w, err)
			return
		}
		seat, err := s.Store.Seat(r.Context(), p.SeatID)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusForbidden, "forbidden", "seat is not active")
			return
		}
		if err != nil {
			s.storeError(w, r, err)
			return
		}
		org, err := s.Store.Organization(r.Context(), seat.OrganizationID)
		if err != nil {
			s.storeError(w, r, err)
			return
		}
		q := &seatReq{principal: p, seat: seat, org: org, execution: r.Header.Get(runtimeapi.HeaderExecution)}
		if h := r.Header.Get(runtimeapi.HeaderGeneration); h != "" {
			gen, err := strconv.ParseInt(h, 10, 64)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid", "invalid "+runtimeapi.HeaderGeneration)
				return
			}
			q.gen, q.hasGen = gen, true
		}
		annotate(r.Context(), "organization_id", seat.OrganizationID, "seat_id", seat.ID, "seat", seat.Key)
		if q.execution != "" {
			annotate(r.Context(), "execution_id", q.execution)
		}
		next(w, r, q)
	})
}

func fenced(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, "fenced", "missing or stale "+runtimeapi.HeaderGeneration)
}

func (s *server) leaseAcquire(w http.ResponseWriter, r *http.Request, q *seatReq) {
	if q.principal.PodUID == "" {
		writeError(w, http.StatusForbidden, "forbidden", "token is not bound to a pod")
		return
	}
	l, err := s.Store.AcquireLease(r.Context(), q.seat.ID, q.principal.PodUID)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "conflict", "lease is held by another pod")
		return
	}
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	s.Log.InfoContext(r.Context(), "lease acquired", "seat_id", q.seat.ID, "generation", l.Generation, "pod_uid", q.principal.PodUID)
	writeJSON(w, http.StatusOK, l)
}

func (s *server) leaseRenew(w http.ResponseWriter, r *http.Request, q *seatReq) {
	var req runtimeapi.LeaseRenewRequest
	if !decodeBody(w, r, &req) {
		return
	}
	l, err := s.Store.RenewLease(r.Context(), q.seat.ID, q.principal.PodUID, req.Generation)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *server) leaseRelease(w http.ResponseWriter, r *http.Request, q *seatReq) {
	f, ok := q.fence()
	if !ok {
		fenced(w)
		return
	}
	if err := s.Store.ReleaseLease(r.Context(), f.SeatID, f.Generation); err != nil {
		s.storeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) state(w http.ResponseWriter, r *http.Request, q *seatReq) {
	var req runtimeapi.StateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if !q.hasGen {
		q.gen, q.hasGen = req.Generation, true
	}
	f, _ := q.fence()
	switch req.State {
	case "Warm", "Executing", "Quiescing", "Stopped":
	default:
		writeError(w, http.StatusBadRequest, "invalid", "state must be Warm, Executing, Quiescing or Stopped")
		return
	}
	if err := s.Store.ReportState(r.Context(), f, req); err != nil {
		s.storeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) self(w http.ResponseWriter, _ *http.Request, q *seatReq) {
	writeJSON(w, http.StatusOK, tools.Self(q.seat, q.org))
}

func (s *server) listTools(w http.ResponseWriter, _ *http.Request, q *seatReq) {
	writeJSON(w, http.StatusOK, s.Tools.List(q.seat, q.org))
}

func (s *server) callTool(w http.ResponseWriter, r *http.Request, q *seatReq) {
	name := r.PathValue("name")
	var req runtimeapi.ToolCallRequest
	if !decodeBody(w, r, &req) {
		return
	}
	annotate(r.Context(), "tool", name)
	res, err := s.Tools.Call(r.Context(), name, &tools.Call{Seat: q.seat, Org: q.org, Generation: q.gen, HasGeneration: q.hasGen,
		ExecutionID: q.execution, Args: req.Arguments})
	switch {
	case errors.Is(err, tools.ErrUnknownTool):
		writeError(w, http.StatusNotFound, "not_found", "unknown tool "+name)
	case err != nil:
		s.storeError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

const maxWait = 30 * time.Second

// inboxNext long-polls for the next delivery. It answers 204 when nothing
// became eligible within the wait.
func (s *server) inboxNext(w http.ResponseWriter, r *http.Request, q *seatReq) {
	f, ok := q.fence()
	if !ok {
		fenced(w)
		return
	}
	wait := time.Duration(0)
	if v := r.URL.Query().Get("wait"); v != "" {
		secs, err := strconv.Atoi(v)
		if err != nil || secs < 0 {
			writeError(w, http.StatusBadRequest, "invalid", "wait must be a number of seconds")
			return
		}
		wait = min(time.Duration(secs)*time.Second, maxWait)
	}
	deadline := time.Now().Add(wait)
	for {
		d, err := s.Store.LeaseNext(r.Context(), f)
		if err != nil {
			s.storeError(w, r, err)
			return
		}
		if d != nil {
			annotate(r.Context(), "message_id", d.Message.MessageID, "execution_id", d.ExecutionID, "delivery_id", d.DeliveryID)
			writeJSON(w, http.StatusOK, d)
			return
		}
		if !time.Now().Before(deadline) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(min(s.PollInterval, time.Until(deadline))):
		}
	}
}

func (s *server) inboxAck(w http.ResponseWriter, r *http.Request, q *seatReq) {
	f, ok := q.fence()
	if !ok {
		fenced(w)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", "invalid delivery id")
		return
	}
	var req runtimeapi.InboxAckRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Outcome == "" {
		req.Outcome = "completed"
	}
	if req.Outcome != "completed" && req.Outcome != "failed" {
		writeError(w, http.StatusBadRequest, "invalid", "outcome must be completed or failed")
		return
	}
	annotate(r.Context(), "delivery_id", id, "execution_id", req.ExecutionID, "outcome", req.Outcome)
	if err := s.Store.Ack(r.Context(), f, id, req); err != nil {
		s.storeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// events accepts a single ExecutionEvent or an array of them.
func (s *server) events(w http.ResponseWriter, r *http.Request, q *seatReq) {
	f, ok := q.fence()
	if !ok {
		fenced(w)
		return
	}
	var raw json.RawMessage
	if !decodeBody(w, r, &raw) {
		return
	}
	var evs []runtimeapi.ExecutionEvent
	var err error
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '[' {
		err = json.Unmarshal(raw, &evs)
	} else {
		var ev runtimeapi.ExecutionEvent
		err = json.Unmarshal(raw, &ev)
		evs = []runtimeapi.ExecutionEvent{ev}
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", "invalid events: "+err.Error())
		return
	}
	for _, ev := range evs {
		if ev.Kind == "" {
			writeError(w, http.StatusBadRequest, "invalid", "event kind is required")
			return
		}
	}
	annotate(r.Context(), "execution_id", r.PathValue("id"), "events", len(evs))
	if err := s.Store.AppendEvents(r.Context(), f, r.PathValue("id"), evs); err != nil {
		s.storeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) checkpoint(w http.ResponseWriter, r *http.Request, q *seatReq) {
	f, ok := q.fence()
	if !ok {
		fenced(w)
		return
	}
	var cp runtimeapi.Checkpoint
	if !decodeBody(w, r, &cp) {
		return
	}
	if cp.HarnessAdapter == "" || cp.CheckpointRef == "" {
		writeError(w, http.StatusBadRequest, "invalid", "harness_adapter and checkpoint_ref are required")
		return
	}
	switch cp.Guarantee {
	case "":
		cp.Guarantee = "application_checkpoint"
	case "application_checkpoint":
	default:
		writeError(w, http.StatusBadRequest, "invalid", "only application_checkpoint is supported in this release")
		return
	}
	if err := s.Store.SaveCheckpoint(r.Context(), f, cp); err != nil {
		s.storeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// modelProxy forwards inference to the seat's model connection with the
// platform-held credential; the seat token is removed first (§5.2). A seat
// may only request its harness profile's model over its profile's API. Each
// request is recorded as a model_request event on the seat's execution: the
// record of the actual model and destination.
func (s *server) modelProxy(w http.ResponseWriter, r *http.Request, q *seatReq) {
	conn := r.PathValue("connection")
	if !policy.Allows(&q.seat.Manifest, "connection:"+conn, "model.infer") {
		writeError(w, http.StatusForbidden, "forbidden", fmt.Sprintf("model.infer is not granted on connection %s", conn))
		return
	}
	upstream, err := s.Connections.Model(q.seat.OrganizationID, conn)
	if errors.Is(err, connections.ErrNotConfigured) {
		writeError(w, http.StatusServiceUnavailable, "unavailable", err.Error())
		return
	}
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	annotate(r.Context(), "connection", conn)
	path := "/" + r.PathValue("rest")
	api := model.APIForPath(path)
	rec := runtimeapi.ModelRequest{Connection: conn, API: api, UpstreamHost: hostOf(upstream)}
	start := time.Now()
	defer func() {
		rec.DurationMS = time.Since(start).Milliseconds()
		s.recordModelRequest(r, q, rec)
	}()
	reject := func(status int, code, msg string) {
		rec.Status, rec.Rejected = status, msg
		writeError(w, status, code, msg)
	}
	// The body is buffered so a request rejected because the credential was
	// rotated can be sent again, and so its model can be checked; nothing
	// reaches the seat until the retry is decided. Streaming responses
	// still stream.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxModelRequest))
	if err != nil {
		reject(http.StatusRequestEntityTooLarge, "invalid", "model request body too large or unreadable")
		return
	}
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)
	rec.Model, rec.Stream = req.Model, req.Stream
	switch {
	case api == "":
		reject(http.StatusNotFound, "not_found", fmt.Sprintf("%s is not a model API path", path))
		return
	case api == "models":
		if r.Method != http.MethodGet {
			reject(http.StatusMethodNotAllowed, "invalid", "model listing is GET only")
			return
		}
	case r.Method != http.MethodPost:
		reject(http.StatusMethodNotAllowed, "invalid", fmt.Sprintf("%s %s is not allowed", r.Method, path))
		return
	default:
		if msg := modelAllowed(q, conn, api, req.Model); msg != "" {
			reject(http.StatusForbidden, "forbidden", msg)
			return
		}
	}
	forward := func(dst http.ResponseWriter, m connectors.Model) {
		out := r.Clone(r.Context())
		out.Header.Del("Authorization")
		out.Header.Del("X-Api-Key")
		out.Header.Del(runtimeapi.HeaderGeneration)
		out.URL.Path = path
		out.URL.RawPath = ""
		out.RequestURI = ""
		out.Body = io.NopCloser(bytes.NewReader(body))
		out.ContentLength = int64(len(body))
		m.Proxy().ServeHTTP(dst, out)
	}
	mw := &meter{ResponseWriter: w}
	defer func() {
		mw.done()
		rec.Status, rec.Usage = mw.status, mw.usage
	}()
	hold := &holdUnauthorized{ResponseWriter: mw, header: http.Header{}}
	forward(hold, upstream)
	if !hold.rejected {
		return
	}
	if s.Connections.RefreshNow(r.Context(), q.seat.OrganizationID, conn) {
		if next, err := s.Connections.Model(q.seat.OrganizationID, conn); err == nil {
			annotate(r.Context(), "credential_refreshed", true)
			rec.CredentialRefreshed = true
			forward(mw, next)
			return
		}
	}
	hold.replay()
}

// modelAllowed returns why a seat may not request model over api through
// conn, or "" when it may. Through its harness profile's connection a seat
// may only use the profile's model and API; through another connection
// granted model.infer explicitly, any model.
func modelAllowed(q *seatReq, conn, api, id string) string {
	sel := q.seat.Manifest.Harness.Model
	if sel == nil || sel.Connection != conn {
		return ""
	}
	switch {
	case id == "":
		return "the request names no model"
	case id != sel.ID:
		return fmt.Sprintf("model %q is not allowed for this seat; its harness profile selects %q", id, sel.ID)
	case api != sel.API:
		return fmt.Sprintf("API %s is not allowed for this seat; its harness profile uses %s", api, sel.API)
	}
	return ""
}

// hostOf is the upstream host of a model connection, when it reports one.
func hostOf(m connectors.Model) string {
	if h, ok := m.(interface{ Host() string }); ok {
		return h.Host()
	}
	return ""
}

// recordModelRequest adds a model_request event to the seat's execution.
// Requests outside an execution, or from a fenced generation, are only logged.
func (s *server) recordModelRequest(r *http.Request, q *seatReq, rec runtimeapi.ModelRequest) {
	annotate(r.Context(), "model", rec.Model, "api", rec.API, "upstream", rec.UpstreamHost, "upstream_status", rec.Status)
	f, ok := q.fence()
	if q.execution == "" || !ok {
		return
	}
	data, _ := json.Marshal(rec)
	ev := runtimeapi.ExecutionEvent{Kind: runtimeapi.EventModelRequest, Time: time.Now().UTC(), Data: data}
	if err := s.Store.AppendEvents(context.WithoutCancel(r.Context()), f, q.execution, []runtimeapi.ExecutionEvent{ev}); err != nil {
		annotate(r.Context(), "model_request_event_error", err.Error())
	}
}

// maxModelRequest bounds a buffered model request body.
const maxModelRequest = 32 << 20

// holdUnauthorized passes a response through unless its status is 401, in
// which case it holds the response back so the request can be retried with
// a refreshed credential, or replayed unchanged.
type holdUnauthorized struct {
	http.ResponseWriter
	header   http.Header
	decided  bool
	rejected bool
	status   int
	body     bytes.Buffer
}

func (h *holdUnauthorized) Header() http.Header { return h.header }

func (h *holdUnauthorized) WriteHeader(code int) {
	if h.decided {
		return
	}
	h.decided, h.status = true, code
	if code == http.StatusUnauthorized {
		h.rejected = true
		return
	}
	maps.Copy(h.ResponseWriter.Header(), h.header)
	h.ResponseWriter.WriteHeader(code)
}

func (h *holdUnauthorized) Write(b []byte) (int, error) {
	if !h.decided {
		h.WriteHeader(http.StatusOK)
	}
	if h.rejected {
		if h.body.Len() < 64<<10 {
			h.body.Write(b)
		}
		return len(b), nil
	}
	return h.ResponseWriter.Write(b)
}

func (h *holdUnauthorized) Flush() {
	if h.decided && !h.rejected {
		if f, ok := h.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func (h *holdUnauthorized) Unwrap() http.ResponseWriter { return h.ResponseWriter }

// replay writes the held-back 401 response.
func (h *holdUnauthorized) replay() {
	maps.Copy(h.ResponseWriter.Header(), h.header)
	h.ResponseWriter.WriteHeader(h.status)
	_, _ = h.ResponseWriter.Write(h.body.Bytes())
}
