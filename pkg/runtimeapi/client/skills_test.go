package client

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

func TestSkillsBundleEmptySetAndAuthentication(t *testing.T) {
	var gen atomic.Int64
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != runtimeapi.PathSkillsBundle || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer tok-1" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		fmt.Fprint(w, `{"skills":[]}`)
	}), &gen)
	out, err := c.SkillsBundle(context.Background())
	if err != nil || out.Skills == nil || len(out.Skills) != 0 {
		t.Fatalf("empty sync: %+v %v", out, err)
	}
}

func TestSkillsBundleRejectsMissingTruncatedAndOversizeResponses(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"skills":null}`, `{"skills":[`, strings.Repeat(" ", runtimeapi.MaxSkillsBundleBytes+1)} {
		var gen atomic.Int64
		c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }), &gen)
		if _, err := c.SkillsBundle(context.Background()); err == nil {
			t.Fatalf("accepted invalid response (%d bytes)", len(body))
		}
	}
}
