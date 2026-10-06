// Package api serves the platform runtime API (pkg/runtimeapi): /v1/* for
// seats, /internal/v1/* for the controller, health and metrics. Seat and
// controller callers use separate credentials and authentication paths
// (§13.2); every error body is a runtimeapi.Error.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/auth"
	"github.com/darcys22/steadmesh/services/connections"
	"github.com/darcys22/steadmesh/services/metrics"
	"github.com/darcys22/steadmesh/services/store"
	"github.com/darcys22/steadmesh/services/tools"
)

// Config holds the server's dependencies.
type Config struct {
	Store       *store.Store
	Auth        auth.Authenticator
	Tools       *tools.Registry
	Connections *connections.Manager
	Metrics     *metrics.Metrics
	Gatherer    prometheus.Gatherer
	Log         *slog.Logger
	// PollInterval is how often a waiting inbox request re-checks the queue.
	PollInterval time.Duration
}

type server struct{ Config }

const maxBody = 1 << 20

// New returns the platform HTTP handler.
func New(cfg Config) http.Handler {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 500 * time.Millisecond
	}
	s := &server{cfg}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+runtimeapi.PathHealthz, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET "+runtimeapi.PathReadyz, s.readyz)
	mux.Handle("GET /metrics", promhttp.HandlerFor(cfg.Gatherer, promhttp.HandlerOpts{}))

	seat := func(pattern string, h seatHandler) { mux.Handle(pattern, s.seatAuth(h)) }
	seat("POST "+runtimeapi.PathLeaseAcquire, s.leaseAcquire)
	seat("POST "+runtimeapi.PathLeaseRenew, s.leaseRenew)
	seat("POST "+runtimeapi.PathLeaseRelease, s.leaseRelease)
	seat("POST "+runtimeapi.PathState, s.state)
	seat("GET "+runtimeapi.PathSelf, s.self)
	seat("GET "+runtimeapi.PathBootstrap, s.bootstrap)
	seat("GET "+runtimeapi.PathTools, s.listTools)
	seat("POST "+runtimeapi.PathToolCall+"{name}", s.callTool)
	seat("GET "+runtimeapi.PathInboxNext, s.inboxNext)
	seat("POST "+runtimeapi.PathInboxAck+"{id}/ack", s.inboxAck)
	seat("POST "+runtimeapi.PathExecEvents+"{id}/events", s.events)
	seat("PUT "+runtimeapi.PathCheckpoint, s.checkpoint)
	// Claude Code sends HEAD <base>/api/hello as a connectivity check at startup
	// (docs/decisions.html#harness). It reveals nothing, so it is answered
	// locally rather than forwarded upstream.
	mux.HandleFunc("HEAD "+runtimeapi.PathModelProxy+"{connection}/api/hello", func(w http.ResponseWriter, _ *http.Request) {})
	mux.Handle(runtimeapi.PathModelProxy+"{connection}/{rest...}", s.seatAuth(s.modelProxy))

	internal := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.controllerAuth(h)) }
	internal("POST "+runtimeapi.PathInternalSync, s.sync)
	internal("GET "+runtimeapi.PathInternalOrgs+"{id}/runtime", s.runtime)
	internal("POST "+runtimeapi.PathInternalOrgs+"{id}/verify", s.verify)
	internal("DELETE "+runtimeapi.PathInternalOrgs+"{id}", s.deleteOrg)
	internal("POST "+runtimeapi.PathInternalSeats+"{id}/fence", s.fence)
	internal("POST "+runtimeapi.PathInternalSeats+"{id}/probe", s.createProbe)
	internal("GET "+runtimeapi.PathInternalSeats+"{id}/probe/{probe}", s.getProbe)

	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	return s.observe(mux)
}

func (s *server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Store.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "database unreachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// reqInfo collects identifiers for the request log line.
type reqInfo struct{ attrs []any }

type infoKey struct{}

func annotate(ctx context.Context, attrs ...any) {
	if ri, ok := ctx.Value(infoKey{}).(*reqInfo); ok {
		ri.attrs = append(ri.attrs, attrs...)
	}
}

type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// observe logs each request with its organisation, seat and execution ids
// and records request metrics by route pattern (§14).
func (s *server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ri := &reqInfo{}
		rec := &recorder{ResponseWriter: w}
		r = r.WithContext(context.WithValue(r.Context(), infoKey{}, ri))
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		s.Metrics.HTTPRequests.WithLabelValues(route, strconv.Itoa(rec.status)).Inc()
		s.Metrics.HTTPDuration.WithLabelValues(route).Observe(time.Since(start).Seconds())
		if route == "GET /healthz" || route == "GET /readyz" || route == "GET /metrics" {
			return
		}
		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelError
		}
		attrs := append([]any{"method", r.Method, "route", route, "status", rec.status, "duration_ms", time.Since(start).Milliseconds()}, ri.attrs...)
		s.Log.Log(r.Context(), level, "request", attrs...)
	})
}

func (s *server) controllerAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.Auth.Controller(r.Context(), bearer(r)); err != nil {
			authError(w, err)
			return
		}
		next(w, r)
	})
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	return ""
}

func authError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "caller is not authorised")
	case errors.Is(err, auth.ErrUnauthenticated):
		writeError(w, http.StatusUnauthorized, "unauthenticated", "missing or invalid token")
	default:
		writeError(w, http.StatusServiceUnavailable, "unavailable", "authentication unavailable")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, runtimeapi.Error{Code: code, Message: msg})
}

// storeError maps a store or service error to an HTTP error.
func (s *server) storeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrFenced):
		writeError(w, http.StatusConflict, "fenced", "lease generation is not current")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, store.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, context.Canceled):
		writeError(w, http.StatusServiceUnavailable, "unavailable", "request cancelled")
	default:
		s.Log.ErrorContext(r.Context(), "internal error", "route", r.Pattern, "error", err)
		writeError(w, http.StatusInternalServerError, "unavailable", "internal error")
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}
