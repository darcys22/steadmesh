//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// consoleChecks runs after the delegation (A06) and Pod kill (A08) steps. It
// checks the console demo end to end: the handoff and the tracker change are
// in the activity feed, and the killed seat kept its identity and run history.
func consoleChecks(t *testing.T) {
	step(t, "console traces delegation and survives a Pod restart", func(t *testing.T) {
		mustRun(t, nil, "kubectl", "--context", kctx, "-n", systemNS, "rollout", "status", "deploy/steadmesh-console", "--timeout=180s")
		token := strings.TrimSpace(mustRun(t, nil, "kubectl", "--context", kctx, "-n", systemNS, "create", "token", "steadmesh-console"))
		platform := portForward(t, "svc/steadmesh-platform", 8080)
		api := func(path string, out any) int {
			t.Helper()
			req, _ := http.NewRequest("GET", platform+path, nil)
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			b, _ := io.ReadAll(res.Body)
			if res.StatusCode == http.StatusOK && out != nil {
				if err := json.Unmarshal(b, out); err != nil {
					t.Fatalf("%s: %v", path, err)
				}
			}
			return res.StatusCode
		}

		var orgs []runtimeapi.ConsoleOrganization
		if code := api(runtimeapi.PathConsoleOrgs, &orgs); code != http.StatusOK {
			t.Fatalf("console API: %d", code)
		}
		var orgID string
		for _, o := range orgs {
			if o.Namespace == orgNS {
				orgID = o.ID
			}
		}
		if orgID == "" {
			t.Fatalf("organisation in %s not listed: %+v", orgNS, orgs)
		}

		var act runtimeapi.ConsoleActivity
		api(runtimeapi.PathConsoleOrgs+"/"+orgID+"/activity?limit=1000&after="+time.Now().Add(-24*time.Hour).UTC().Format(time.RFC3339Nano), &act)
		var handoff, project bool
		for _, it := range act.Items {
			handoff = handoff || (it.Kind == runtimeapi.ActivityMessage && it.SeatKey == "representative_sean" && it.PeerSeat == "eng_lead")
			project = project || (it.Kind == runtimeapi.ActivityOperation && it.SeatKey == "eng_lead" && strings.HasPrefix(it.Summary, "project.create"))
		}
		if !handoff || !project {
			t.Fatalf("activity is missing the delegation (%t) or the tracker project (%t)", handoff, project)
		}

		seatID := jsonpath(t, "agentseat", seatStatefulSet(t, "representative_sean"), "{.spec.seatID}")
		var seat runtimeapi.ConsoleSeatDetail
		api(runtimeapi.PathConsoleSeats+seatID, &seat)
		gens := map[int64]bool{}
		for _, e := range seat.Executions {
			gens[e.LeaseGeneration] = true
		}
		if seat.Config.SeatID != seatID || len(gens) < 2 {
			t.Fatalf("seat %s history does not span the Pod restart: generations %v", seatID, gens)
		}

		token = ""
		if code := api(runtimeapi.PathConsoleOrgs, nil); code != http.StatusUnauthorized {
			t.Fatalf("anonymous console API request: %d", code)
		}

		ui := portForward(t, "deploy/steadmesh-console", 8090)
		var pages strings.Builder
		for _, p := range []string{"/orgs/" + orgID, "/orgs/" + orgID + "/work", "/orgs/" + orgID + "/activity",
			"/orgs/" + orgID + "/readiness", "/seats/" + seatID} {
			res, err := http.Get(ui + p)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("console %s: %d %s", p, res.StatusCode, b)
			}
			fmt.Fprintf(&pages, "%s\n", b)
		}
		html := pages.String()
		for _, want := range []string{seatID, "eng_lead", "project.create", "OperationalReady"} {
			if !strings.Contains(html, want) {
				t.Errorf("console pages do not mention %q", want)
			}
		}
		for _, secret := range []string{"xoxb-fake-bot-token", "xapp-fake-app-token", "lin_api_fake"} {
			if strings.Contains(html, secret) {
				t.Fatalf("console exposes credential %q", secret)
			}
		}
	})
}
