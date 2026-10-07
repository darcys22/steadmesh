//go:build e2e

package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/harnesses/conformance/modelstub"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// modelSvc is the in-cluster model fake: one endpoint serving the OpenAI
// Responses and Chat Completions APIs (and Anthropic Messages).
const modelSvc = "http://steadmesh-fakes." + systemNS + ".svc:8092/v1"

// mixedHarnesses moves the engineer to Codex (OpenAI Responses) and the
// reviewer to Pi (OpenAI Chat Completions) on the scripted model fake, and
// checks that each real harness passes the readiness probe turn, uses its
// model and endpoint through the platform proxy, does delegated work with
// platform tools, and keeps working after the model key is rotated in its
// Secret, with no Terraform run or restart.
func mixedHarnesses(t *testing.T, env []string) {
	// The fakes were restarted by A25: reconnect.
	f := &fakes{slack: portForward(t, "svc/steadmesh-fakes", 8090), linear: portForward(t, "svc/steadmesh-fakes", 8091)}
	model := portForward(t, "svc/steadmesh-fakes", 8092)
	requests := func(t *testing.T) []modelstub.Request {
		var out []modelstub.Request
		f.get(t, model+"/_test/requests", &out)
		return out
	}
	mixed := append(append([]string{}, env...),
		`TF_VAR_model_connections={"stub":{"adapter":"model","secret_ref":"k8s:stub-credentials","endpoint_ref":"`+modelSvc+`",`+
			`"model":{"apis":["openai_responses","openai_chat"],"models":[{"id":"stub-codex"},{"id":"stub-pi","apis":["openai_chat"]}]}}}`,
		`TF_VAR_seat_harnesses={"engineer":{"adapter":"codex","model":{"connection":"stub","id":"stub-codex","settings":{"reasoning_effort":"low"}}},`+
			`"reviewer":{"adapter":"pi","model":{"connection":"stub","id":"stub-pi"}}}`)

	step(t, "codex and pi seats pass readiness with a model-backed probe turn", func(t *testing.T) {
		f.post(t, model+"/_test/keys", map[string]any{"valid": []string{"stub-key-1"}})
		f.post(t, model+"/_test/reset", map[string]any{})
		mustRun(t, mixed, "make", "-C", "examples", "apply-organisation")
		if code := runCode(t, nil, filepath.Join(root, "bin/orgctl"), "verify", "--context", kctx, "--namespace", orgNS, "--timeout", "6m"); code != 0 {
			t.Fatal("orgctl verify failed with the codex and pi seats")
		}
		// Each probe turn reached the model, through the API the profile
		// selected, with the connection's key injected by the platform.
		probes := map[string]string{}
		for _, r := range requests(t) {
			if strings.Contains(r.LastUser, modelstub.ProbeMarker) {
				probes[r.Model] = r.API
				if r.Key != "stub-key-1" {
					t.Errorf("probe request for %s carried key %q", r.Model, r.Key)
				}
			}
		}
		if probes["stub-codex"] != modelstub.OpenAIResponses || probes["stub-pi"] != modelstub.OpenAIChat {
			t.Fatalf("probe turns by model: %v", probes)
		}
	})

	step(t, "codex and pi seats do delegated work with platform tools", func(t *testing.T) {
		// /delegate passes every following line on, so each delegation is its own message.
		f.dm(t, userSean, "/delegate eng_lead /task\n"+
			`/delegate engineer CALL memory.write {"store":"engineering","path":"notes/codex.md","text":"written by codex"}`, "")
		f.dm(t, userSean, "/delegate eng_lead /task\n"+
			`/delegate reviewer CALL memory.write {"store":"engineering","path":"notes/pi.md","text":"written by pi"}`, "")
		for path, want := range map[string]string{"notes/codex.md": "written by codex", "notes/pi.md": "written by pi"} {
			waitFor(t, path+" in team memory", 5*time.Minute, func() bool {
				out := f.askRep(t, `/tool memory.read {"store":"engineering","path":"`+path+`"}`, "tool memory.read")
				return strings.Contains(out, want)
			})
		}
	})

	step(t, "model requests record the model and endpoint actually used", func(t *testing.T) {
		evs := modelRequests(t, "engineer")
		var codex bool
		for _, e := range evs {
			codex = codex || (e.Model == "stub-codex" && e.API == "openai_responses" && e.UpstreamHost == "steadmesh-fakes."+systemNS+".svc:8092" && e.Status == 200)
			if e.Model != "" && e.Model != "stub-codex" {
				t.Errorf("engineer requested model %q", e.Model)
			}
		}
		if !codex {
			t.Fatalf("no successful stub-codex model_request on the engineer's runs: %+v", evs)
		}
		var pi bool
		for _, e := range modelRequests(t, "reviewer") {
			pi = pi || (e.Model == "stub-pi" && e.API == "openai_chat" && e.Status == 200)
		}
		if !pi {
			t.Fatal("no successful stub-pi model_request on the reviewer's runs")
		}
	})

	step(t, "rotating the model key mid-session needs no restart", func(t *testing.T) {
		pod := seatStatefulSet(t, "engineer") + "-0"
		uid := mustRun(t, nil, "kubectl", "--context", kctx, "-n", orgNS, "get", "pod", pod, "-o", "jsonpath={.metadata.uid}")
		before := len(requests(t))
		mustRun(t, nil, "sh", "-c", "kubectl --context "+kctx+" -n "+systemNS+" create secret generic stub-credentials --from-literal=api_key=stub-key-2 --dry-run=client -o yaml | kubectl --context "+kctx+" apply -f -")
		f.post(t, model+"/_test/keys", map[string]any{"valid": []string{"stub-key-2"}})
		f.dm(t, userSean, "/delegate eng_lead /task\n"+
			`/delegate engineer CALL memory.write {"store":"engineering","path":"notes/rotated.md","text":"after rotation"}`, "")
		waitFor(t, "work after rotation", 5*time.Minute, func() bool {
			out := f.askRep(t, `/tool memory.read {"store":"engineering","path":"notes/rotated.md"}`, "tool memory.read")
			return strings.Contains(out, "after rotation")
		})
		if now := mustRun(t, nil, "kubectl", "--context", kctx, "-n", orgNS, "get", "pod", pod, "-o", "jsonpath={.metadata.uid}"); now != uid {
			t.Fatal("the engineer's Pod was replaced")
		}
		// The fake rejects the old key (recorded without a model); the
		// platform reloads the Secret and retries with the new one.
		var rejected, accepted int
		for _, r := range requests(t)[before:] {
			switch {
			case r.Key == "stub-key-1" && r.Model != "":
				t.Errorf("the old key was accepted after rotation: %+v", r)
			case r.Key == "stub-key-1":
				rejected++
			case r.Key == "stub-key-2" && r.Model == "stub-codex":
				accepted++
			}
		}
		if accepted == 0 {
			t.Fatal("no request with the rotated key reached the model")
		}
		t.Logf("after rotation: %d requests rejected with the old key, %d served with the new key", rejected, accepted)
	})
}

// modelRequests returns the model_request events on a seat's runs, read
// through the console API.
func modelRequests(t *testing.T, seatKey string) []runtimeapi.ModelRequest {
	t.Helper()
	token := strings.TrimSpace(mustRun(t, nil, "kubectl", "--context", kctx, "-n", systemNS, "create", "token", "steadmesh-console"))
	platform := portForward(t, "svc/steadmesh-platform", 8080)
	get := func(path string, out any) {
		req, _ := http.NewRequest("GET", platform+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", path, res.StatusCode, b)
		}
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	seatID := jsonpath(t, "agentseat", seatStatefulSet(t, seatKey), "{.spec.seatID}")
	var seat runtimeapi.ConsoleSeatDetail
	get(runtimeapi.PathConsoleSeats+seatID, &seat)
	var out []runtimeapi.ModelRequest
	for _, e := range seat.Executions {
		var d runtimeapi.ConsoleExecutionDetail
		get(runtimeapi.PathConsoleExecutions+e.ID, &d)
		for _, ev := range d.Events {
			if ev.Kind == runtimeapi.EventModelRequest {
				var mr runtimeapi.ModelRequest
				_ = json.Unmarshal(ev.Data, &mr)
				out = append(out, mr)
			}
		}
	}
	return out
}
