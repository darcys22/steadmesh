package console

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/darcys22/steadmesh/api/v1alpha1"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

//go:embed web
var webFS embed.FS

// Config holds the server's dependencies.
type Config struct {
	Platform Platform
	Cluster  Cluster
	Log      *slog.Logger
	// PollInterval is how often the live activity stream polls the platform.
	PollInterval time.Duration
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
}

type server struct {
	Config
	pages map[string]*template.Template
}

// New returns the console HTTP handler.
func New(cfg Config) (http.Handler, error) {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	pages, err := parsePages()
	if err != nil {
		return nil, err
	}
	s := &server{Config: cfg, pages: pages}
	static, err := fs.Sub(webFS, "web/static")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /orgs/{id}", s.organization)
	mux.HandleFunc("GET /orgs/{id}/grid", s.grid)
	mux.HandleFunc("GET /orgs/{id}/work", s.work)
	mux.HandleFunc("GET /orgs/{id}/activity", s.activity)
	mux.HandleFunc("GET /orgs/{id}/stream", s.stream)
	mux.HandleFunc("GET /orgs/{id}/readiness", s.readiness)
	mux.HandleFunc("GET /seats/{id}", s.seat)
	mux.HandleFunc("GET /runs/{id}", s.run)
	mux.HandleFunc("GET /conversations/{id}", s.conversation)
	return securityHeaders(mux), nil
}

// securityHeaders keeps the console's pages from being framed or loading
// anything but their own static assets.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func parsePages() (map[string]*template.Template, error) {
	base, err := template.New("").Funcs(funcs).ParseFS(webFS, "web/templates/layout.html", "web/templates/partials.html")
	if err != nil {
		return nil, err
	}
	names, err := fs.Glob(webFS, "web/templates/page_*.html")
	if err != nil {
		return nil, err
	}
	pages := map[string]*template.Template{}
	for _, n := range names {
		t, err := template.Must(base.Clone()).ParseFS(webFS, n)
		if err != nil {
			return nil, err
		}
		pages[n[len("web/templates/page_"):len(n)-len(".html")]] = t
	}
	pages["partials"] = base
	return pages, nil
}

func (s *server) render(w http.ResponseWriter, r *http.Request, page string, data any) {
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, "layout", data); err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

func (s *server) fragment(name string, data any) (string, error) {
	var buf bytes.Buffer
	err := s.pages["partials"].ExecuteTemplate(&buf, name, data)
	return buf.String(), err
}

func (s *server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	s.Log.ErrorContext(r.Context(), "console request failed", "path", r.URL.Path, "error", err)
	http.Error(w, "the platform or cluster could not be read; see the console logs", http.StatusBadGateway)
}

// orgHeader is shared by every organisation page.
type orgHeader struct {
	Org   *runtimeapi.ConsoleOrganization
	CRD   *v1alpha1.AgentOrganization
	Ready string
	Tab   string
}

// orgState is everything the organisation-level pages combine.
type orgState struct {
	orgHeader
	Seats  []seatView
	ByKey  map[string]seatView
	Counts map[string]int
}

func (s *server) loadOrg(ctx context.Context, orgID, tab string) (*orgState, error) {
	org, err := s.Platform.Organization(ctx, orgID)
	if err != nil {
		return nil, err
	}
	statuses, err := s.Platform.Seats(ctx, orgID)
	if err != nil {
		return nil, err
	}
	crd, err := s.Cluster.Organization(ctx, orgID)
	if err != nil {
		return nil, err
	}
	crdSeats := map[string]v1alpha1.AgentSeat{}
	pods := map[string]corev1.Pod{}
	if crd != nil {
		if crdSeats, err = s.Cluster.Seats(ctx, crd); err != nil {
			return nil, err
		}
		if pods, err = s.Cluster.Pods(ctx, crd); err != nil {
			return nil, err
		}
	}
	byID := map[string]runtimeapi.ConsoleSeatStatus{}
	for _, st := range statuses {
		byID[st.SeatID] = st
	}
	out := &orgState{orgHeader: orgHeader{Org: org, CRD: crd, Ready: operationalReady(crd), Tab: tab},
		ByKey: map[string]seatView{}, Counts: map[string]int{}}
	now := s.Now()
	for _, cfg := range org.Seats {
		var c *v1alpha1.AgentSeat
		if v, ok := crdSeats[cfg.SeatID]; ok {
			c = &v
		}
		var p *corev1.Pod
		if v, ok := pods[cfg.Key]; ok {
			p = &v
		}
		sv := newSeatView(cfg, byID[cfg.SeatID], c, p, now)
		out.Seats = append(out.Seats, sv)
		out.ByKey[cfg.Key] = sv
		out.Counts[sv.State]++
	}
	return out, nil
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	orgs, err := s.Platform.Organizations(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(orgs) == 1 {
		http.Redirect(w, r, "/orgs/"+orgs[0].ID, http.StatusFound)
		return
	}
	type row struct {
		Org   runtimeapi.ConsoleOrganization
		Ready string
	}
	var rows []row
	for _, o := range orgs {
		crd, err := s.Cluster.Organization(r.Context(), o.ID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		rows = append(rows, row{Org: o, Ready: operationalReady(crd)})
	}
	s.render(w, r, "index", map[string]any{"Orgs": rows})
}

func (s *server) organization(w http.ResponseWriter, r *http.Request) {
	st, err := s.loadOrg(r.Context(), r.PathValue("id"), "organisation")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "organisation", map[string]any{"H": st.orgHeader, "S": st,
		"Teams": groupByTeam(st.Org.Teams, st.Seats), "Now": s.Now()})
}

// grid re-renders the seat grid for the organisation page's periodic refresh.
func (s *server) grid(w http.ResponseWriter, r *http.Request) {
	st, err := s.loadOrg(r.Context(), r.PathValue("id"), "organisation")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	html, err := s.fragment("seat-grid", map[string]any{"S": st, "Teams": groupByTeam(st.Org.Teams, st.Seats), "Now": s.Now()})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(html))
}

func (s *server) work(w http.ResponseWriter, r *http.Request) {
	ctx, orgID := r.Context(), r.PathValue("id")
	st, err := s.loadOrg(ctx, orgID, "work")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	seat, status := r.URL.Query().Get("seat"), r.URL.Query().Get("status")
	ops, err := s.Platform.Operations(ctx, orgID, seat, status)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	arts, err := s.Platform.Artifacts(ctx, orgID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	runs, err := s.Platform.Executions(ctx, orgID, "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if seat != "" {
		runs = slices.DeleteFunc(runs, func(e runtimeapi.ConsoleExecution) bool { return e.SeatKey != seat })
		arts = slices.DeleteFunc(arts, func(a runtimeapi.ConsoleArtifact) bool { return a.OwnerSeat != seat })
	}
	var blockers []runtimeapi.ConsoleOperation
	for _, op := range ops {
		if op.Status == "unknown" || op.Status == "failed" {
			blockers = append(blockers, op)
		}
	}
	s.render(w, r, "work", map[string]any{"H": st.orgHeader, "S": st, "Operations": ops, "Blockers": blockers,
		"Artifacts": arts, "Runs": runs, "Seat": seat, "Status": status, "Now": s.Now()})
}

func (s *server) activity(w http.ResponseWriter, r *http.Request) {
	ctx, orgID := r.Context(), r.PathValue("id")
	st, err := s.loadOrg(ctx, orgID, "activity")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	items, err := s.Platform.Activity(ctx, orgID, time.Time{}, 150)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	convs, err := s.Platform.Conversations(ctx, orgID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var cursor time.Time
	if len(items) > 0 {
		cursor = items[len(items)-1].At
	} else {
		cursor = s.Now()
	}
	slices.Reverse(items) // newest first on the page
	s.render(w, r, "activity", map[string]any{"H": st.orgHeader, "S": st, "Map": layoutMap(st.Seats), "Items": items,
		"Conversations": convs, "Cursor": cursor.UTC().Format(time.RFC3339Nano), "Now": s.Now()})
}

func (s *server) readiness(w http.ResponseWriter, r *http.Request) {
	ctx, orgID := r.Context(), r.PathValue("id")
	st, err := s.loadOrg(ctx, orgID, "readiness")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	claims := map[string]corev1.PersistentVolumeClaimPhase{}
	if st.CRD != nil {
		if claims, err = s.Cluster.Claims(ctx, st.CRD.Namespace); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	var rows []readinessRow
	for _, sv := range st.Seats {
		rows = append(rows, newReadinessRow(sv, claims))
	}
	var conds []conditionView
	if st.CRD != nil {
		conds = conditions(st.CRD.Status.Conditions)
	}
	s.render(w, r, "readiness", map[string]any{"H": st.orgHeader, "S": st, "Conditions": conds, "Rows": rows, "Now": s.Now()})
}

func (s *server) seat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, err := s.Platform.Seat(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	st, err := s.loadOrg(ctx, d.Config.OrganizationID, "organisation")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sv := st.ByKey[d.Config.Key]
	sv.Status = d.Status
	s.render(w, r, "seat", map[string]any{"H": st.orgHeader, "Seat": sv, "D": d, "Now": s.Now()})
}

func (s *server) run(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, err := s.Platform.Execution(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	seat, err := s.Platform.Seat(ctx, d.Execution.SeatID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	data := map[string]any{"D": d, "Steps": pairEvents(d.Events), "Now": s.Now()}
	if seat != nil && err == nil {
		st, err := s.loadOrg(ctx, seat.Config.OrganizationID, "activity")
		if err != nil {
			s.fail(w, r, err)
			return
		}
		data["H"] = st.orgHeader
	}
	s.render(w, r, "run", data)
}

func (s *server) conversation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, err := s.Platform.Conversation(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	data := map[string]any{"D": d, "Now": s.Now()}
	if orgID := r.URL.Query().Get("org"); orgID != "" {
		st, err := s.loadOrg(ctx, orgID, "activity")
		if err != nil {
			s.fail(w, r, err)
			return
		}
		data["H"] = st.orgHeader
	}
	s.render(w, r, "conversation", data)
}

// step is a run event, with a tool request paired with its result.
type step struct {
	Event  runtimeapi.ConsoleEvent
	Result *runtimeapi.ConsoleEvent
}

// pairEvents attaches each tool_result to the preceding tool_request with
// the same correlation id; unpaired events stand alone.
func pairEvents(events []runtimeapi.ConsoleEvent) []step {
	var out []step
	open := map[string]int{}
	for _, ev := range events {
		if ev.Kind == "tool_result" && ev.CorrelationID != "" {
			if i, ok := open[ev.CorrelationID]; ok {
				out[i].Result = &ev
				delete(open, ev.CorrelationID)
				continue
			}
		}
		if ev.Kind == "tool_request" && ev.CorrelationID != "" {
			open[ev.CorrelationID] = len(out)
		}
		out = append(out, step{Event: ev})
	}
	return out
}
