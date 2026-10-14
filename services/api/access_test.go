package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/darcys22/steadmesh/pkg/access"
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/store"
)

func TestAccessDeduplicatesSandboxConnectionWiring(t *testing.T) {
	for _, a := range []*access.SeatAccess{
		nil,
		{},
		{GitHub: []access.GitHubGrant{
			{Connection: "github", Host: "github.com", Delivery: access.DeliveryPlatform, Repos: []string{"acme/private"}},
			{Connection: "github", Host: "github.com", Delivery: access.DeliverySandbox, Repos: []string{"acme/a"}},
			{Connection: "github", Host: "github.com", Delivery: access.DeliverySandbox, Repos: []string{"acme/b"}},
			{Connection: "ghe", Host: "git.example.com", Delivery: access.DeliverySandbox},
		}},
	} {
		rec := httptest.NewRecorder()
		q := &seatReq{seat: &store.Seat{ID: "seat-id", Key: "worker", Manifest: compile.SeatManifest{Access: a}}}
		(&server{}).access(rec, httptest.NewRequest(http.MethodGet, "/access", nil), q)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var got runtimeapi.AccessResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if a.Empty() {
			if len(got.GitHub) != 0 {
				t.Fatalf("empty access has wiring: %+v", got)
			}
			continue
		}
		want := []runtimeapi.GitHubSandbox{{Connection: "ghe", Host: "git.example.com"}, {Connection: "github", Host: "github.com"}}
		if !slices.Equal(got.GitHub, want) {
			t.Fatalf("wiring %+v, want %+v", got.GitHub, want)
		}
	}
}
