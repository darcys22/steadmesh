//go:build e2e

package e2e

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/harnesses/claudecode"
	"github.com/darcys22/steadmesh/harnesses/codex"
	"github.com/darcys22/steadmesh/harnesses/pi"
	fakegithub "github.com/darcys22/steadmesh/tests/fakes/github"
)

// TestDemo is the completion demo (examples/mixed-harness): an organisation
// whose engineering team runs Claude Code on Anthropic Messages (lead), Codex
// on OpenAI Responses (engineer, the only seat with GitHub access) and Pi on
// an OpenAI Chat Completions endpoint (reviewer). Sean's representative uses
// the deterministic fake harness so the run is reproducible and its checks
// are exact.
//
//  1. The team collaborates through messages and a shared note, with no
//     Linear and no work items.
//  2. It does the same with work items, and the engineer opens a pull request.
//  3. Work items are published to Linear (the fake); Linear goes down, the
//     organisation stays ready, and on reconnect nothing is duplicated.
//  4. The Anthropic key is rotated in its Kubernetes Secret mid-session; the
//     lead keeps working without a Terraform run or a restart.
//
// DEMO_MODE=fakes (default) uses the scripted model fake and fake GitHub;
// every model turn is scripted with CALL lines. DEMO_MODE=live uses the real
// endpoints and a real GitHub repository and gives the team its tasks in
// plain language (see examples/mixed-harness/README.md for the credentials).
// The results are written to docs/demo/<mode>-results.json and rendered with
// every recorded mode into docs/demo-results.html.
//
//	make demo                  (fakes)
//	make demo DEMO_MODE=live   (real services)
func TestDemo(t *testing.T) {
	if !strings.HasPrefix(kctx, "kind-") {
		t.Fatalf("refusing to run against non-kind context %q", kctx)
	}
	mode := envOr("DEMO_MODE", "fakes")
	d := &demo{t: t, mode: mode, res: demoResult{Mode: mode, StartedAt: time.Now().UTC(),
		Versions: map[string]string{"claude-code": claudecode.PinnedVersion, "codex": codex.PinnedVersion, "pi": pi.PinnedVersion}}}
	defer d.write()
	cfg := d.config()
	d.env = append([]string{
		"TF_VAR_slack_endpoint_ref=" + fakeSvc + ":8090",
		"TF_VAR_linear_endpoint_ref=" + fakeSvc + ":8091",
		"TF_VAR_seat_idle_timeout=10m",
	}, cfg...)

	d.run("bring up the platform and the mixed organisation", func(t *testing.T) string {
		mustRun(t, d.env, "make", "-C", "examples", "apply-foundation")
		mustRun(t, nil, "kubectl", "--context", kctx, "apply", "-f", "tests/e2e/manifests/fakes.yaml")
		mustRun(t, nil, "kubectl", "--context", kctx, "-n", systemNS, "rollout", "status", "deploy/steadmesh-fakes", "--timeout=180s")
		// The port-forwards outlive this step: they belong to the demo.
		d.f = &fakes{slack: portForward(d.t, "svc/steadmesh-fakes", 8090), linear: portForward(d.t, "svc/steadmesh-fakes", 8091)}
		d.model = portForward(d.t, "svc/steadmesh-fakes", 8092)
		d.gh = portForward(d.t, "svc/steadmesh-fakes", 8093)
		d.githubSecret(t)
		if mode == "fakes" {
			d.f.post(t, d.model+"/_test/keys", map[string]any{"valid": []string{"demo-anthropic-1", "demo-openai", "demo-i14"}})
		}
		mustRun(t, d.env, "make", "-C", "examples", "apply")
		return "each seat passed readiness with a probe turn through its own harness and model"
	})

	d.run("1. collaborate through messages and a shared note (no Linear, no work items)", func(t *testing.T) string {
		d.delegate(t, d.collaborate())
		note := d.waitRead(t, `{"store":"engineering","path":"notes/demo-login.md"}`, 8*time.Minute, func(out string) bool {
			return strings.Contains(out, "engineer") && strings.Contains(out, "reviewer") && strings.Count(out, "\\n") >= 2
		})
		return "shared note notes/demo-login.md written by the lead, the engineer and the reviewer: " + excerpt(note)
	})

	d.run("2. the same with work items; the engineer opens a pull request", func(t *testing.T) string {
		d.delegate(t, d.workFlow())
		out := d.waitRead(t, "", 10*time.Minute, func(out string) bool {
			return strings.Contains(out, `"owner":"engineer"`) && (strings.Contains(out, `"status":"done"`) || strings.Contains(out, `"status":"in_review"`))
		})
		pr := d.pullRequest(t)
		return fmt.Sprintf("engineering/W-1 owned by the engineer, %s; pull request %s", statusOf(out), pr)
	})

	d.run("3. publish work items to Linear; an outage never blocks the organisation", func(t *testing.T) string {
		withLinear := append(append([]string{}, d.env...), "TF_VAR_enable_linear=true", "TF_VAR_publish_work_to_tracker=true")
		mustRun(t, withLinear, "make", "-C", "examples", "apply-organisation")
		waitFor(t, "W-1 published once", 3*time.Minute, func() bool { return d.issues(t, "engineering/W-1: ") == 1 })
		d.f.post(t, d.f.linear+"/_test/keys", map[string]any{"valid": []string{"nobody-has-this-key"}})
		if code := runCode(t, nil, filepath.Join(root, "bin/orgctl"), "verify", "--context", kctx, "--namespace", orgNS, "--timeout", "5m"); code != 0 {
			t.Fatal("verification failed while only Linear was down")
		}
		waitFor(t, "IntegrationsDegraded", 2*time.Minute, func() bool {
			return jsonpath(t, "agentorganization", orgName, `{.status.conditions[?(@.type=="IntegrationsDegraded")].status}`) == "True"
		})
		d.delegate(t, d.secondItem())
		d.waitRead(t, `{"work_id":"engineering/W-2"}`, 6*time.Minute, func(out string) bool { return strings.Contains(out, "release notes") })
		if n := d.issues(t, "engineering/W-2: "); n != 0 {
			t.Fatalf("published while Linear rejected every key: %d", n)
		}
		d.f.post(t, d.f.linear+"/_test/keys", map[string]any{"valid": []string{}})
		waitFor(t, "W-2 published after reconnecting", 6*time.Minute, func() bool { return d.issues(t, "engineering/W-2: ") == 1 })
		if n := d.issues(t, "engineering/W-1: "); n != 1 {
			t.Fatalf("W-1 published %d times", n)
		}
		return "W-1 published once; while Linear was down the organisation stayed ready (IntegrationsDegraded) and W-2 was created internally; after reconnecting W-2 was published, nothing twice"
	})

	d.run("4. rotate the Anthropic key mid-session", func(t *testing.T) string {
		pod := seatStatefulSet(t, "eng_lead") + "-0"
		uid := mustRun(t, nil, "kubectl", "--context", kctx, "-n", orgNS, "get", "pod", pod, "-o", "jsonpath={.metadata.uid}")
		newKey := "demo-anthropic-2"
		if mode == "live" {
			newKey = os.Getenv("ANTHROPIC_API_KEY_2")
			if newKey == "" {
				t.Skip("set ANTHROPIC_API_KEY_2 to rotate the Anthropic key in the live demo")
			}
		}
		applySecret(t, "anthropic-credentials", map[string]string{"api_key": newKey})
		if mode == "fakes" {
			d.f.post(t, d.model+"/_test/keys", map[string]any{"valid": []string{newKey, "demo-openai", "demo-i14"}})
		}
		d.delegate(t, d.afterRotation())
		d.waitRead(t, `{"store":"engineering","path":"notes/demo-login.md"}`, 5*time.Minute, func(out string) bool { return strings.Contains(out, "after rotation") })
		if now := mustRun(t, nil, "kubectl", "--context", kctx, "-n", orgNS, "get", "pod", pod, "-o", "jsonpath={.metadata.uid}"); now != uid {
			t.Fatal("the lead's Pod was replaced")
		}
		return "the lead kept working with the rotated key: no Terraform run, no restart (Pod " + pod + " unchanged)"
	})

	d.run("record the models and endpoints each seat actually used", func(t *testing.T) string {
		for _, s := range []string{"eng_lead", "engineer", "reviewer"} {
			d.res.Seats = append(d.res.Seats, d.seatSummary(t, s))
		}
		return fmt.Sprintf("%d seats; see the table", len(d.res.Seats))
	})
}

type demo struct {
	t     *testing.T
	mode  string
	env   []string
	f     *fakes
	model string
	gh    string
	res   demoResult
}

type demoResult struct {
	Mode       string            `json:"mode"`
	StartedAt  time.Time         `json:"started_at"`
	FinishedAt time.Time         `json:"finished_at"`
	Versions   map[string]string `json:"versions"`
	Config     json.RawMessage   `json:"config"`
	Steps      []demoStep        `json:"steps"`
	Seats      []demoSeat        `json:"seats"`
}

type demoStep struct {
	Name     string `json:"name"`
	Passed   bool   `json:"passed"`
	Skipped  bool   `json:"skipped,omitempty"`
	Seconds  int    `json:"seconds"`
	Evidence string `json:"evidence"`
}

type demoSeat struct {
	Key      string       `json:"key"`
	Harness  string       `json:"harness"`
	Requests []demoTarget `json:"requests"`
}

type demoTarget struct {
	Connection   string `json:"connection"`
	API          string `json:"api"`
	Model        string `json:"model"`
	UpstreamHost string `json:"upstream_host"`
	Count        int    `json:"count"`
	OK           int    `json:"ok"`
	Refreshed    int    `json:"credential_refreshed,omitempty"`
}

// run runs a step and records its outcome.
func (d *demo) run(name string, fn func(t *testing.T) string) {
	start := time.Now()
	var evidence string
	ok := d.t.Run(name, func(t *testing.T) { evidence = fn(t) })
	st := demoStep{Name: name, Passed: ok, Seconds: int(time.Since(start).Seconds()), Evidence: evidence}
	if ok && evidence == "" {
		st.Skipped = true
	}
	d.res.Steps = append(d.res.Steps, st)
	if !ok {
		d.t.FailNow()
	}
}

// config reads the mode's variables (examples/mixed-harness) and returns
// them as TF_VAR_ environment entries. The recorded copy has no secrets.
func (d *demo) config() []string {
	t := d.t
	file := filepath.Join(root, "examples", "mixed-harness", "fakes.json")
	if d.mode == "live" {
		file = filepath.Join(root, "examples", "mixed-harness", "live.json.example")
		for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "I14_API_KEY", "GITHUB_TOKEN", "GITHUB_TEST_REPO"} {
			if os.Getenv(k) == "" {
				t.Skipf("the live demo needs %s (examples/mixed-harness/README.md)", k)
			}
		}
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if d.mode == "live" {
		b = []byte(strings.ReplaceAll(string(b), "OWNER/TEST-REPO", os.Getenv("GITHUB_TEST_REPO")))
	}
	var vars map[string]json.RawMessage
	if err := json.Unmarshal(b, &vars); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	d.res.Config = b
	if d.mode == "live" {
		keys, _ := json.Marshal(map[string]string{"anthropic": os.Getenv("ANTHROPIC_API_KEY"), "openai": os.Getenv("OPENAI_API_KEY"), "i14": os.Getenv("I14_API_KEY")})
		vars["model_api_keys"] = keys
	}
	var out []string
	for k, v := range vars {
		var s string
		if json.Unmarshal(v, &s) == nil {
			out = append(out, "TF_VAR_"+k+"="+s)
			continue
		}
		out = append(out, "TF_VAR_"+k+"="+string(v))
	}
	sort.Strings(out)
	return out
}

// githubSecret creates the github connection's credential: a GitHub App
// registered with the fake, or the live PAT.
func (d *demo) githubSecret(t *testing.T) {
	if d.mode == "live" {
		applySecret(t, "github-credentials", map[string]string{"token": os.Getenv("GITHUB_TOKEN")})
		return
	}
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pub, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	d.f.post(t, d.gh+"/_test/app", map[string]any{"app_id": "4242", "installation_id": "77",
		"public_key": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub}))})
	applySecret(t, "github-credentials", map[string]string{"app_id": "4242", "installation_id": "77",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))})
}

// delegate gives the lead a task through Sean's representative.
func (d *demo) delegate(t *testing.T, task string) {
	t.Helper()
	d.f.dm(t, userSean, "/delegate eng_lead "+task, "")
}

// waitRead has the representative read memory (args for memory.read) or the
// work item list (no args) until ok accepts the result.
func (d *demo) waitRead(t *testing.T, args string, timeout time.Duration, ok func(string) bool) string {
	t.Helper()
	tool := "memory.read"
	switch {
	case args == "":
		tool, args = "work.list", `{"store":"engineering"}`
	case strings.Contains(args, "work_id"):
		tool = "work.get"
	}
	var last string
	waitFor(t, tool+" "+args, timeout, func() bool {
		last = d.f.askRep(t, "/tool "+tool+" "+args, "tool "+tool)
		return ok(last)
	})
	return last
}

// msg is a messages.send call carrying the next seat's scripted turn.
func msg(to string, calls ...string) string {
	return call("messages.send", map[string]string{"to": to, "body": strings.Join(calls, "\n")})
}

func (d *demo) collaborate() string {
	if d.mode == "live" {
		return "Plan a login page with your team, using a shared note and messages only (no work items). Write a short plan to " +
			"notes/demo-login.md in the engineering memory store with memory.write. Then message the engineer and the reviewer and ask " +
			"each to append one line to that note with memory.append saying what they will do; the engineer's line should start with " +
			"'engineer:' and the reviewer's with 'reviewer:'."
	}
	note := func(who, text string) string {
		return call("memory.append", map[string]string{"store": "engineering", "path": "notes/demo-login.md", "text": who + ": " + text + "\n"})
	}
	return strings.Join([]string{
		call("memory.write", map[string]string{"store": "engineering", "path": "notes/demo-login.md", "text": "# Login page\nlead: plan agreed - engineer builds, reviewer reviews\n"}),
		msg("engineer", note("engineer", "building the form and session handling"),
			msg("reviewer", note("reviewer", "will review the form for accessibility and errors"),
				msg("eng_lead", "Review done: the login plan is approved."))),
	}, "\n")
}

func (d *demo) workFlow() string {
	if d.mode == "live" {
		repo := os.Getenv("GITHUB_TEST_REPO")
		return "Create a work item in the engineering store for 'Add a signup page' with the acceptance criterion 'a pull request " +
			"exists in " + repo + "' (work.create). Ask the engineer to claim it, then in the workspace clone " + repo + " with git, create " +
			"a branch, add a short docs/signup.md, push it and open a pull request with gh; then record the pull request URL as " +
			"evidence and set the item to in_review (work.update). Ask the reviewer to read the pull request description and tell the " +
			"engineer whether it is acceptable; the engineer then marks the item done with the review as evidence."
	}
	id := "engineering/W-1"
	pr := call("connections.invoke", map[string]any{"connection": "github", "operation": "pull_request.create",
		"params": map[string]string{"repo": "acme/sandbox", "title": "Add a signup page", "head": "signup", "base": "main", "body": "Implements engineering/W-1."}})
	return strings.Join([]string{
		call("work.create", map[string]any{"store": "engineering", "objective": "Add a signup page", "acceptance": []string{"a pull request exists in acme/sandbox"}}),
		msg("engineer",
			call("work.claim", map[string]any{"work_id": id, "expected_revision": 1, "note": "taking this"}),
			call("work.update_plan", map[string]any{"work_id": id, "expected_revision": 2, "plan": []map[string]string{{"step": "build the form", "status": "completed"}, {"step": "open a pull request", "status": "in_progress"}}}),
			pr,
			call("work.update", map[string]any{"work_id": id, "expected_revision": 3, "status": "in_review", "evidence": []string{"pull request in acme/sandbox"}}),
			msg("reviewer",
				call("work.update", map[string]any{"work_id": id, "expected_revision": 4, "note": "reviewed: approved"}),
				msg("engineer", call("work.update", map[string]any{"work_id": id, "expected_revision": 5, "status": "done", "evidence": []string{"review approved by the reviewer"}})))),
	}, "\n")
}

func (d *demo) secondItem() string {
	if d.mode == "live" {
		return "Create a work item in the engineering store for 'Write the release notes' (work.create). Nothing else is needed."
	}
	return call("work.create", map[string]any{"store": "engineering", "objective": "Write the release notes"})
}

func (d *demo) afterRotation() string {
	if d.mode == "live" {
		return "Append the line 'lead: after rotation' to notes/demo-login.md in the engineering store (memory.append)."
	}
	return call("memory.append", map[string]string{"store": "engineering", "path": "notes/demo-login.md", "text": "lead: after rotation\n"})
}

// issues counts tracker issues whose title starts with prefix.
func (d *demo) issues(t *testing.T, prefix string) int {
	var st struct {
		Issues []struct {
			Title string `json:"title"`
		} `json:"issues"`
	}
	d.f.get(t, d.f.linear+"/_test/state", &st)
	n := 0
	for _, is := range st.Issues {
		if strings.HasPrefix(is.Title, prefix) {
			n++
		}
	}
	return n
}

var prURL = regexp.MustCompile(`https://github\.com/[^\s"]+/pull/\d+`)

// pullRequest returns the pull request the engineer opened.
func (d *demo) pullRequest(t *testing.T) string {
	if d.mode == "live" {
		out := d.waitRead(t, `{"work_id":"engineering/W-1"}`, time.Minute, func(out string) bool { return prURL.MatchString(out) })
		return prURL.FindString(out)
	}
	var st fakegithub.State
	d.f.get(t, d.gh+"/_test/state", &st)
	for _, p := range st.Pulls {
		if p.Title == "Add a signup page" && p.Repo == "acme/sandbox" {
			return fmt.Sprintf("acme/sandbox#%d (fake GitHub, opened through the platform)", p.Number)
		}
	}
	t.Fatal("no pull request in acme/sandbox")
	return ""
}

// seatSummary groups a seat's model_request events by destination.
func (d *demo) seatSummary(t *testing.T, key string) demoSeat {
	harness := map[string]string{"eng_lead": "claude-code", "engineer": "codex", "reviewer": "pi"}[key]
	byTarget := map[string]*demoTarget{}
	for _, r := range modelRequests(t, key) {
		if r.Rejected != "" || r.API == "models" {
			continue
		}
		k := r.Connection + "|" + r.API + "|" + r.Model + "|" + r.UpstreamHost
		tg, ok := byTarget[k]
		if !ok {
			tg = &demoTarget{Connection: r.Connection, API: r.API, Model: r.Model, UpstreamHost: r.UpstreamHost}
			byTarget[k] = tg
		}
		tg.Count++
		if r.Status/100 == 2 {
			tg.OK++
		}
		if r.CredentialRefreshed {
			tg.Refreshed++
		}
	}
	s := demoSeat{Key: key, Harness: harness}
	for _, tg := range byTarget {
		s.Requests = append(s.Requests, *tg)
	}
	sort.Slice(s.Requests, func(i, j int) bool { return s.Requests[i].Count > s.Requests[j].Count })
	if len(s.Requests) == 0 {
		t.Fatalf("%s made no model requests", key)
	}
	return s
}

func excerpt(s string) string {
	var r struct {
		Text string `json:"text"`
	}
	if i := strings.Index(s, "{"); i >= 0 && json.Unmarshal([]byte(s[i:]), &r) == nil && r.Text != "" {
		s = r.Text
	}
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " / ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

func statusOf(out string) string {
	if m := regexp.MustCompile(`"status":"([a-z_]+)"`).FindStringSubmatch(out); m != nil {
		return "status " + m[1]
	}
	return "status unknown"
}

// write records the results and re-renders docs/demo-results.html from every
// recorded mode.
func (d *demo) write() {
	d.res.FinishedAt = time.Now().UTC()
	dir := filepath.Join(root, "docs", "demo")
	_ = os.MkdirAll(dir, 0o755)
	b, _ := json.MarshalIndent(d.res, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, d.mode+"-results.json"), append(b, '\n'), 0o644)
	var runs []demoResult
	for _, m := range []string{"fakes", "live"} {
		raw, err := os.ReadFile(filepath.Join(dir, m+"-results.json"))
		if err != nil {
			continue
		}
		var r demoResult
		if json.Unmarshal(raw, &r) == nil {
			runs = append(runs, r)
		}
	}
	f, err := os.Create(filepath.Join(root, "docs", "demo-results.html"))
	if err != nil {
		d.t.Log(err)
		return
	}
	defer f.Close()
	if err := demoPage.Execute(f, map[string]any{"Runs": runs, "Live": hasMode(runs, "live")}); err != nil {
		d.t.Log(err)
	}
}

func hasMode(runs []demoResult, mode string) bool {
	for _, r := range runs {
		if r.Mode == mode {
			return true
		}
	}
	return false
}

var demoPage = template.Must(template.New("demo").Funcs(template.FuncMap{
	"stamp": func(t time.Time) string { return t.Format("2006-01-02 15:04 UTC") },
	"pretty": func(b json.RawMessage) string {
		var v any
		if json.Unmarshal(b, &v) != nil {
			return string(b)
		}
		out, _ := json.MarshalIndent(v, "", "  ")
		return string(out)
	},
}).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Demo results · Steadmesh</title>
<meta name="description" content="Results of the mixed-harness completion demo: what ran, the models and endpoints each seat actually used, and the outcome of each step.">
<link rel="stylesheet" href="assets/style.css">
</head>
<body>
<div class="layout">
<aside class="sidebar">
  <button class="menu-toggle" type="button" aria-label="Toggle navigation">Menu</button>
  <a class="brand" href="index.html">Steadmesh</a>
  <div class="brand-sub">Platform documentation · v0.1</div>
  <nav>
    <div class="nav-group">Get started</div>
    <ul>
      <li><a href="index.html">Overview</a></li>
      <li><a href="quickstart.html">Quickstart</a></li>
      <li><a href="from-source.html">Run from source</a></li>
      <li><a href="tutorial.html">Tutorial</a></li>
    </ul>
    <div class="nav-group">Guides</div>
    <ul>
      <li><a href="real-services.html">Connect real services</a></li>
      <li><a href="operations.html">Operating an organisation</a></li>
    </ul>
    <div class="nav-group">Reference</div>
    <ul>
      <li><a href="configuration.html">Configuration</a></li>
      <li><a href="harnesses.html">Harnesses and models</a></li>
      <li><a href="sandbox.html">Sandbox access</a></li>
      <li><a href="tools.html">Agent tools</a></li>
      <li><a href="architecture.html">Architecture</a></li>
      <li><a href="decisions.html">Design decisions</a></li>
      <li><a href="status.html">Acceptance status</a></li>
      <li><a href="demo-results.html">Demo results</a></li>
    </ul>
  </nav>
</aside>
<main>
<h1>Demo results</h1>
<p>The completion demo (<code>examples/mixed-harness</code>, <code>make demo</code>) runs an organisation whose engineering team uses three harnesses on three model endpoints: Claude Code on Anthropic Messages (lead), Codex on OpenAI Responses (engineer, the only seat with GitHub access) and Pi on an OpenAI Chat Completions endpoint (reviewer). The team collaborates without Linear, then with work items and a pull request; work is then published to Linear through an outage; finally the Anthropic key is rotated mid-session. This page is generated by the run.</p>
<p>Runs against fakes and against real services are recorded separately. A fakes run uses a scripted model endpoint, so it proves the wiring (harness, model routing, tools, permissions, recovery), not model quality.</p>
{{if not .Live}}<div class="callout"><p><strong>Real services:</strong> not recorded yet. Run <code>make demo DEMO_MODE=live</code> with the credentials listed in <code>examples/mixed-harness/README.md</code>; this page then shows both runs.</p></div>{{end}}
{{range .Runs}}
<h2>{{if eq .Mode "live"}}Real services{{else}}Fakes{{end}} run, {{stamp .StartedAt}}</h2>
<h3>Pinned versions</h3>
<table><tr><th>Harness</th><th>Version</th></tr>
{{range $k, $v := .Versions}}<tr><td><code>{{$k}}</code></td><td>{{$v}}</td></tr>{{end}}
</table>
<h3>Steps</h3>
<table><tr><th>Step</th><th>Result</th><th>Time</th><th>Evidence</th></tr>
{{range .Steps}}<tr><td>{{.Name}}</td><td>{{if .Skipped}}skipped{{else if .Passed}}passed{{else}}<strong>failed</strong>{{end}}</td><td>{{.Seconds}}s</td><td>{{.Evidence}}</td></tr>{{end}}
</table>
<h3>Models and endpoints actually used</h3>
<p>From the <code>model_request</code> events the platform recorded for each seat's runs.</p>
<table><tr><th>Seat</th><th>Harness</th><th>Connection</th><th>API</th><th>Model</th><th>Upstream host</th><th>Requests (2xx)</th></tr>
{{range $s := .Seats}}{{range .Requests}}<tr><td><code>{{$s.Key}}</code></td><td>{{$s.Harness}}</td><td>{{.Connection}}</td><td><code>{{.API}}</code></td><td><code>{{.Model}}</code></td><td><code>{{.UpstreamHost}}</code></td><td>{{.Count}} ({{.OK}}{{if .Refreshed}}, {{.Refreshed}} after a credential refresh{{end}})</td></tr>{{end}}{{end}}
</table>
<details><summary>Configuration (examples/mixed-harness)</summary><pre><code>{{pretty .Config}}</code></pre></details>
{{end}}
<p class="footer">Steadmesh documentation. Generated by <code>tests/e2e/demo_test.go</code>.</p>
</main>
</div>
<script src="assets/site.js"></script>
</body>
</html>
`))
