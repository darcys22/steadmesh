//go:build e2e

package e2e

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	fakegithub "github.com/darcys22/steadmesh/tests/fakes/github"
)

// ghSvc is the in-cluster fake GitHub (REST API and a test page).
const ghSvc = "http://steadmesh-fakes." + systemNS + ".svc:8093"

// sandboxAccess grants the reviewer (Pi on the browser image) sandbox
// access through plugins and checks each one in the cluster:
//   - github with sandbox delivery: a scoped App token reaches the
//     granted repository through the egress gateway; revoking the grant
//     revokes the token at GitHub;
//   - github with platform delivery: the lead opens a pull request through
//     the gateway, limited to the granted repository;
//   - egress: an ungranted host is denied and recorded; removing a host
//     applies live, without replacing the Pod;
//   - browser: the seat opens a page with the granted signed-in session;
//   - the NetworkPolicy lets only seats that use it reach the gateway.
func sandboxAccess(t *testing.T, env []string) {
	f := &fakes{slack: portForward(t, "svc/steadmesh-fakes", 8090), linear: portForward(t, "svc/steadmesh-fakes", 8091)}
	gh := portForward(t, "svc/steadmesh-fakes", 8093)
	state := func(t *testing.T) fakegithub.State {
		var st fakegithub.State
		f.get(t, gh+"/_test/state", &st)
		return st
	}

	step(t, "configure the fake GitHub App and the browser session", func(t *testing.T) {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		pub, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
		f.post(t, gh+"/_test/app", map[string]any{"app_id": "4242", "installation_id": "77",
			"public_key": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub}))})
		priv := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
		applySecret(t, "github-credentials", map[string]string{"app_id": "4242", "installation_id": "77", "private_key": priv})
		session, _ := json.Marshal(map[string]any{"cookies": []map[string]any{{"name": "session", "value": "e2e-user",
			"domain": "steadmesh-fakes." + systemNS + ".svc", "path": "/", "expires": -1, "httpOnly": true, "secure": false, "sameSite": "Lax"}}, "origins": []any{}})
		applySecret(t, "websession-credentials", map[string]string{"storage_state": string(session)})
	})

	profiles := func(withExtra bool) string {
		p := map[string]any{
			"github_dev": map[string]any{
				"tools":  map[string]any{"binaries": []string{"git", "gh", "curl"}},
				"github": map[string]any{"connection": "github", "repos": []string{"acme/sandbox"}, "permissions": map[string]string{"contents": "read", "pull_requests": "write"}, "delivery": "sandbox"},
			},
			"lead_github": map[string]any{"github": map[string]any{"connection": "github", "repos": []string{"acme/sandbox"}, "permissions": map[string]string{"pull_requests": "write"}}},
			"web":         map[string]any{"browser": map[string]any{"session": map[string]any{"connection": "websession"}}},
			"linear_api":  map[string]any{"egress": map[string]any{"hosts": []string{"steadmesh-fakes." + systemNS + ".svc:8091"}}},
		}
		seats := map[string][]string{"reviewer": {"github_dev", "web"}, "eng_lead": {"lead_github"}}
		if withExtra {
			seats["reviewer"] = append(seats["reviewer"], "linear_api")
		}
		pb, _ := json.Marshal(p)
		sb, _ := json.Marshal(seats)
		return "TF_VAR_access_profiles=" + string(pb) + "\x00TF_VAR_seat_access=" + string(sb)
	}
	conns := `TF_VAR_extra_connections={"github":{"adapter":"github","secret_ref":"k8s:github-credentials","endpoint_ref":"` + ghSvc + `"},` +
		`"websession":{"adapter":"browser_session","secret_ref":"k8s:websession-credentials"}}`
	harness := `TF_VAR_seat_harnesses={"engineer":{"adapter":"codex","model":{"connection":"stub","id":"stub-codex"}},` +
		`"reviewer":{"adapter":"pi","image":"steadmesh/seat-pi-browser:dev","model":{"connection":"stub","id":"stub-pi"}}}`
	withAccess := func(extra bool) []string {
		out := append(append([]string{}, env...), conns, harness)
		return append(out, strings.Split(profiles(extra), "\x00")...)
	}

	step(t, "access profiles apply and the reviewer's access is in effect", func(t *testing.T) {
		mustRun(t, withAccess(true), "make", "-C", "examples", "apply-organisation")
		if code := runCode(t, nil, filepath.Join(root, "bin/orgctl"), "verify", "--context", kctx, "--namespace", orgNS, "--timeout", "6m"); code != 0 {
			t.Fatal("orgctl verify failed with access profiles")
		}
		waitFor(t, "AccessApplied on the reviewer", 3*time.Minute, func() bool {
			return jsonpath(t, "agentseat", seatStatefulSet(t, "reviewer"), `{.status.conditions[?(@.type=="AccessApplied")].status}`) == "True"
		})
		// Only seats that use the gateway may reach it.
		reviewerNP := mustRun(t, nil, "kubectl", "--context", kctx, "-n", orgNS, "get", "networkpolicy", seatStatefulSet(t, "reviewer"), "-o", "json")
		engineerNP := mustRun(t, nil, "kubectl", "--context", kctx, "-n", orgNS, "get", "networkpolicy", seatStatefulSet(t, "engineer"), "-o", "json")
		compact := func(s string) string { return strings.Join(strings.Fields(s), "") }
		if !strings.Contains(compact(reviewerNP), `"steadmesh.io/component":"egress"`) || strings.Contains(compact(engineerNP), `"steadmesh.io/component":"egress"`) {
			t.Fatal("NetworkPolicy: only the reviewer may reach the egress gateway")
		}
	})

	var reviewerPod string
	step(t, "a scoped GitHub token reaches the granted repository through the gateway", func(t *testing.T) {
		reviewerPod = mustRun(t, nil, "kubectl", "--context", kctx, "-n", orgNS, "get", "pod", seatStatefulSet(t, "reviewer")+"-0", "-o", "jsonpath={.metadata.uid}")
		allowed := `curl -sf -o /dev/null -H "Authorization: Bearer $(steadmesh-tools credential token)" ` + ghSvc + `/repos/acme/sandbox`
		ungranted := `curl -s -o /dev/null -H "Authorization: Bearer $(steadmesh-tools credential token)" ` + ghSvc + `/repos/acme/other`
		denied := `curl -s -o /dev/null http://steadmesh-fakes.` + systemNS + `.svc:8090/_test/posted`
		linear := `curl -s -o /dev/null http://steadmesh-fakes.` + systemNS + `.svc:8091/_test/state`
		delegateCalls(t, f, "reviewer", call("bash", map[string]string{"command": allowed}), call("bash", map[string]string{"command": ungranted}),
			call("bash", map[string]string{"command": denied}), call("bash", map[string]string{"command": linear}))
		waitFor(t, "the reviewer's GitHub requests", 4*time.Minute, func() bool {
			var ok, other bool
			for _, r := range state(t).Requests {
				ok = ok || (r.Path == "/repos/acme/sandbox" && strings.HasPrefix(r.Auth, "inst:") && r.Status == 200)
				other = other || (r.Path == "/repos/acme/other" && strings.HasPrefix(r.Auth, "inst:"))
			}
			if other {
				for _, r := range state(t).Requests {
					if r.Path == "/repos/acme/other" && r.Status == 200 {
						t.Fatal("the token reached an ungranted repository")
					}
				}
			}
			return ok && other
		})
		tokens := state(t).Tokens
		if len(tokens) == 0 || strings.Join(tokens[len(tokens)-1].Repos, ",") != "sandbox" {
			t.Fatalf("minted tokens %+v", tokens)
		}
		waitFor(t, "an egress_denied event for the Slack fake", 2*time.Minute, func() bool {
			for _, e := range seatEvents(t, "reviewer", runtimeapi.EventEgressDenied) {
				if strings.Contains(string(e), ":8090") || strings.Contains(string(e), `"port":8090`) {
					return true
				}
			}
			return false
		})
		for _, e := range seatEvents(t, "reviewer", runtimeapi.EventEgressDenied) {
			if strings.Contains(string(e), `"port":8091`) {
				t.Fatalf("the granted Linear host was denied: %s", e)
			}
		}
		issued := seatEvents(t, "reviewer", runtimeapi.EventCredentialIssued)
		if len(issued) == 0 || !strings.Contains(string(issued[0]), "acme/sandbox") || strings.Contains(string(issued[0]), "ghs_") {
			t.Fatalf("credential_issued events %s", issued)
		}
	})

	step(t, "the browser opens a page with the granted signed-in session", func(t *testing.T) {
		delegateCalls(t, f, "reviewer", call("browser_navigate", map[string]string{"url": ghSvc + "/ui/whoami"}))
		waitFor(t, "a signed-in visit", 4*time.Minute, func() bool {
			var visits []string
			f.get(t, gh+"/_test/visits", &visits)
			for _, v := range visits {
				if v == "e2e-user" {
					return true
				}
			}
			return false
		})
	})

	step(t, "platform-delivered GitHub operations stay within the grant", func(t *testing.T) {
		create := func(repo, title string) string {
			return fmt.Sprintf(`/tool connections.invoke {"connection":"github","operation":"pull_request.create","params":{"repo":%q,"title":%q,"head":"feature","base":"main"}}`, repo, title)
		}
		f.dm(t, userSean, "/delegate eng_lead /task\n"+create("acme/sandbox", "E2E change")+"\n"+create("acme/other", "Not granted")+"\n/report", "")
		waitFor(t, "the pull request", 3*time.Minute, func() bool {
			for _, p := range state(t).Pulls {
				if p.Title == "E2E change" && p.Repo == "acme/sandbox" && strings.Contains(p.Body, "steadmesh-op:") {
					return true
				}
			}
			return false
		})
		for _, p := range state(t).Pulls {
			if p.Repo == "acme/other" {
				t.Fatal("a pull request was opened in an ungranted repository")
			}
		}
	})

	step(t, "removing an egress host applies live; removing a GitHub grant revokes its token", func(t *testing.T) {
		mustRun(t, withAccess(false), "make", "-C", "examples", "apply-organisation")
		// Egress applies within seconds and needs no new Pod.
		time.Sleep(10 * time.Second)
		// The browser's own background requests are denied too, so count
		// only denials of the removed host.
		denials := func() int {
			n := 0
			for _, e := range seatEvents(t, "reviewer", runtimeapi.EventEgressDenied) {
				if strings.Contains(string(e), `"port":8091`) {
					n++
				}
			}
			return n
		}
		before := denials()
		delegateCalls(t, f, "reviewer", call("bash", map[string]string{"command": `curl -s -o /dev/null http://steadmesh-fakes.` + systemNS + `.svc:8091/_test/state`}))
		waitFor(t, "the removed host to be denied", 3*time.Minute, func() bool { return denials() > before })
		if uid := mustRun(t, nil, "kubectl", "--context", kctx, "-n", orgNS, "get", "pod", seatStatefulSet(t, "reviewer")+"-0", "-o", "jsonpath={.metadata.uid}"); uid != reviewerPod {
			t.Fatal("an egress change replaced the reviewer's Pod")
		}

		// The GitHub grant goes: its delivered token is revoked at GitHub.
		seat := map[string][]string{"reviewer": {"web"}, "eng_lead": {"lead_github"}}
		sb, _ := json.Marshal(seat)
		out := withAccess(false)
		out = append(out, "TF_VAR_seat_access="+string(sb))
		mustRun(t, out, "make", "-C", "examples", "apply-organisation")
		waitFor(t, "the delivered token to be revoked", 2*time.Minute, func() bool {
			tokens := state(t).Tokens
			for _, tk := range tokens {
				if len(tk.Repos) > 0 && !tk.Revoked && tk.ExpiresAt.After(time.Now()) {
					return false
				}
			}
			return len(tokens) > 0
		})
	})
}

// call is one scripted model tool call (see modelstub).
func call(tool string, args any) string {
	b, _ := json.Marshal(args)
	return "CALL " + tool + " " + string(b)
}

// delegateCalls has the lead pass scripted model calls to a model-backed seat.
func delegateCalls(t *testing.T, f *fakes, seat string, calls ...string) {
	t.Helper()
	f.dm(t, userSean, "/delegate eng_lead /task\n/delegate "+seat+" "+strings.Join(calls, "\n"), "")
}

// applySecret creates or updates a Secret in the control-plane namespace.
func applySecret(t *testing.T, name string, data map[string]string) {
	t.Helper()
	args := []string{"--context", kctx, "-n", systemNS, "create", "secret", "generic", name, "--dry-run=client", "-o", "yaml"}
	for k, v := range data {
		args = append(args, "--from-literal="+k+"="+v)
	}
	y, err := exec.Command("kubectl", args...).Output()
	if err != nil {
		t.Fatalf("render secret %s: %v", name, err)
	}
	cmd := exec.Command("kubectl", "--context", kctx, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(string(y))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("apply secret %s: %v\n%s", name, err, out)
	}
}

// seatEvents returns the data of a seat's execution events of one kind,
// read through the console API.
func seatEvents(t *testing.T, seatKey, kind string) []json.RawMessage {
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
		_ = json.Unmarshal(b, out)
	}
	seatID := jsonpath(t, "agentseat", seatStatefulSet(t, seatKey), "{.spec.seatID}")
	var seat runtimeapi.ConsoleSeatDetail
	get(runtimeapi.PathConsoleSeats+seatID, &seat)
	var out []json.RawMessage
	for _, e := range seat.Executions {
		var d runtimeapi.ConsoleExecutionDetail
		get(runtimeapi.PathConsoleExecutions+e.ID, &d)
		for _, ev := range d.Events {
			if ev.Kind == kind {
				out = append(out, ev.Data)
			}
		}
	}
	return out
}
