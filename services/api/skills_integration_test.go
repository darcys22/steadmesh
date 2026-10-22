//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/skills"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/auth"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/darcys22/steadmesh/services/internal/pgtest"
	"github.com/darcys22/steadmesh/services/metrics"
	"github.com/darcys22/steadmesh/services/store"
	"github.com/darcys22/steadmesh/services/tools"
	"github.com/prometheus/client_golang/prometheus"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

type testSkillCatalog struct {
	source    spec.SkillMarketplace
	onResolve func()
	size      int
	fail      bool
	calls     int
}

func (c *testSkillCatalog) List(_ context.Context, key string, source spec.SkillMarketplace) ([]skills.Entry, error) {
	if source != c.source {
		return nil, fmt.Errorf("unexpected source")
	}
	return []skills.Entry{{Ref: spec.SkillRef{Marketplace: key, Plugin: "p", Skill: "static"}, Description: "static"}, {Ref: spec.SkillRef{Marketplace: key, Plugin: "p", Skill: "dynamic"}, Description: "dynamic"}, {Ref: spec.SkillRef{Marketplace: key, Plugin: "external"}, Unsupported: "remote plugins are unsupported"}}, nil
}
func (c *testSkillCatalog) Resolve(_ context.Context, key string, source spec.SkillMarketplace, ref spec.SkillRef) (skills.Bundle, error) {
	c.calls++
	if source != c.source || key != "market" || ref.Marketplace != key || c.fail {
		return skills.Bundle{}, fmt.Errorf("cannot resolve skill")
	}
	if c.onResolve != nil {
		f := c.onResolve
		c.onResolve = nil
		f()
	}
	body := []byte("---\nname: " + ref.Skill + "\n---\nInstructions\n")
	if c.size > 0 {
		body = bytes.Repeat([]byte("x"), c.size)
	}
	return skills.Bundle{Ref: ref, Files: map[string][]byte{"SKILL.md": body}, Digest: strings.Repeat("a", 64)}, nil
}

func TestSkillsAPIEndToEndPolicyModesAndRevocation(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := orgfixture.Manifest(t)
	sm := m.Seats["lead"]
	source := spec.SkillMarketplace{Repository: "acme/skills", Ref: strings.Repeat("a", 40)}
	static := spec.SkillRef{Marketplace: "market", Plugin: "p", Skill: "static"}
	sm.Skills = &spec.SkillPolicy{Mode: "explicit", Marketplaces: []string{"market"}, Install: []spec.SkillRef{static}, MaxDynamicSkills: 1}
	sm.SkillMarketplaces = map[string]spec.SkillMarketplace{"market": source}
	m.Seats["lead"] = sm
	syncOrg := func() *runtimeapi.SyncResponse {
		out, err := db.SyncOrganization(ctx, store.SyncInput{Namespace: "acme", Key: "acme", SourceUID: "uid", Manifest: m})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	res := syncOrg()
	id := res.Seats["lead"].SeatID
	lease, err := db.AcquireLease(ctx, id, "pod")
	if err != nil {
		t.Fatal(err)
	}
	authn := auth.NewFake("controller")
	authn.AddSeat("seat-token", auth.Seat{SeatID: id, PodUID: "pod"})
	registry := prometheus.NewRegistry()
	met := metrics.New(registry)
	catalog := &testSkillCatalog{source: source}
	reg := tools.New(tools.Deps{Store: db, Metrics: met, SkillCatalog: catalog})
	h := New(Config{Store: db, Auth: authn, Tools: reg, Metrics: met, Gatherer: registry, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	request := func(method, path, body, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set(runtimeapi.HeaderGeneration, strconv.FormatInt(lease.Generation, 10))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	tool := func(name, args string) runtimeapi.ToolCallResult {
		t.Helper()
		w := request("POST", runtimeapi.PathToolCall+name, `{"arguments":`+args+`}`, "seat-token")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
		var out runtimeapi.ToolCallResult
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	fetch := func() *httptest.ResponseRecorder {
		return request("GET", runtimeapi.PathSkillsBundle, "", "seat-token")
	}
	if w := request("GET", runtimeapi.PathSkillsBundle, "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated bundle status %d", w.Code)
	}
	w := fetch()
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"owner":"terraform"`) {
		t.Fatalf("static sync: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("skill bundle may be cached")
	}
	args := `{"marketplace":"market","plugin":"p","skill":"dynamic"}`
	if got := tool("skills.list", `{}`); got.IsError || strings.Contains(string(got.Content), `"skill":"dynamic"`) {
		t.Fatalf("explicit discovery: %+v", got)
	}
	if got := tool("skills.read", args); !got.IsError {
		t.Fatal("explicit mode read unselected skill")
	}
	if got := tool("skills.install", args); !got.IsError {
		t.Fatal("explicit mode installed runtime skill")
	}
	sm.Skills.Mode = "discover"
	m.Seats["lead"] = sm
	syncOrg()
	if got := tool("skills.list", `{}`); got.IsError || !strings.Contains(string(got.Content), `"skill":"dynamic"`) || !strings.Contains(string(got.Content), `unsupported`) {
		t.Fatalf("discover listing: %+v", got)
	}
	if got := tool("skills.read", args); got.IsError {
		t.Fatalf("discover read denied: %+v", got)
	}
	if got := tool("skills.install", args); !got.IsError {
		t.Fatal("discover mutated state")
	}
	sm.Skills.Mode = "agent_choice"
	m.Seats["lead"] = sm
	syncOrg()
	catalog.size = 7 << 20
	if got := tool("skills.install", args); !got.IsError {
		t.Fatal("oversize candidate was durably installed")
	}
	snapshot, err := db.SkillsSnapshot(ctx, res.OrganizationID, id)
	if err != nil || len(snapshot.Selections) != 1 {
		t.Fatalf("failed candidate altered selections: %+v %v", snapshot, err)
	}
	catalog.size = 0
	if got := tool("skills.install", args); got.IsError {
		t.Fatalf("dynamic install: %+v", got)
	}
	w = fetch()
	var bundle runtimeapi.SkillsBundleResponse
	if err := json.Unmarshal(w.Body.Bytes(), &bundle); err != nil || len(bundle.Skills) != 2 {
		t.Fatalf("dynamic sync: %s %v", w.Body.String(), err)
	}
	if got := tool("skills.remove", `{"marketplace":"market","plugin":"p","skill":"static"}`); !got.IsError {
		t.Fatal("removed Terraform-owned skill")
	}
	catalog.size = 7 << 20
	if w := fetch(); w.Code != http.StatusBadRequest {
		t.Fatalf("oversize bundle accepted: %d", w.Code)
	}
	catalog.size = 0
	catalog.fail = true
	if w := fetch(); w.Code != http.StatusBadRequest {
		t.Fatalf("partial/invalid bundle accepted: %d", w.Code)
	}
	catalog.fail = false
	catalog.onResolve = func() { sm.Skills = nil; m.Seats["lead"] = sm; syncOrg() }
	if w := fetch(); w.Code != http.StatusConflict {
		t.Fatalf("authorization changed during fetch: %d %s", w.Code, w.Body.String())
	}
	before := catalog.calls
	w = fetch()
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"skills":[]}` || catalog.calls != before {
		t.Fatalf("disabled sync not empty/offline: %d %s", w.Code, w.Body.String())
	}
}
