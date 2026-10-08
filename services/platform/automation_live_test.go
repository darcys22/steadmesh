//go:build integration && live

package platform_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/connectors/model"
	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/harnesses/claudecode"
	"github.com/darcys22/steadmesh/harnesses/codex"
	"github.com/darcys22/steadmesh/harnesses/conformance"
	"github.com/darcys22/steadmesh/harnesses/pi"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
)

// TestLiveAutomations has real models, through the real harnesses, act as a
// representative whose human asks for reminders and recurring checks, then
// changes, pauses, resumes and cancels them. It runs against a real platform
// (tools, store and scheduler) and checks the automations the model leaves
// behind, so it measures whether models understand the tool interface.
// Each case is skipped without its credential or harness CLI.
func TestLiveAutomations(t *testing.T) {
	for _, tc := range []struct {
		name, bin, keyEnv, adapter, api, model string
		newAdapter                             func() harnesses.Adapter
	}{
		{"claude-code", "claude", "ANTHROPIC_API_KEY", "anthropic", harnesses.APIAnthropicMessages,
			envOr("ANTHROPIC_MODEL", "claude-haiku-4-5-20251001"), func() harnesses.Adapter { return claudecode.New() }},
		{"codex", "codex", "OPENAI_API_KEY", "openai", harnesses.APIOpenAIResponses,
			envOr("OPENAI_MODEL", "gpt-5-mini"), func() harnesses.Adapter { return codex.New() }},
		{"pi", "pi", "ANTHROPIC_API_KEY", "anthropic", harnesses.APIAnthropicMessages,
			envOr("ANTHROPIC_MODEL", "claude-haiku-4-5-20251001"), func() harnesses.Adapter { return pi.New() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := os.Getenv(tc.keyEnv)
			if key == "" {
				t.Skipf("needs %s", tc.keyEnv)
			}
			bin := conformance.HarnessBin(t, tc.bin)
			conn, err := model.New(connectors.Config{Key: "llm", Adapter: tc.adapter, Secret: map[string]string{"api_key": key},
				ModelUses: []connectors.ModelUse{{ID: tc.model, API: tc.api}}})
			if err != nil {
				t.Fatal(err)
			}
			runAutomationScenario(t, tc.newAdapter(), bin, conn, tc.model, tc.api)
		})
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func runAutomationScenario(t *testing.T, a harnesses.Adapter, bin string, conn connectors.Model, modelID, api string) {
	e := newEnv(t)
	sp := orgfixture.Spec()
	b := sp.ChannelBindings["alice"]
	b.Timezone = "Australia/Melbourne"
	sp.ChannelBindings["alice"] = b
	e.sync(sp)
	rep := e.seat("rep_a")
	_, raw := rep.do("GET", runtimeapi.PathBootstrap, nil)
	var boot runtimeapi.Bootstrap
	if err := json.Unmarshal(raw, &boot); err != nil {
		t.Fatal(err)
	}

	dirs := conformance.NewDirs(t, rep.token)
	chain := conformance.NewProxyChain(t, dirs.TokenFile, rep.token, rep.gen, conn.Proxy())
	env := harnesses.Environment{
		OrganizationID: e.org, SeatID: boot.Self.SeatID, SeatKey: "rep_a", DisplayName: "Alice's representative",
		ConfigRevision: boot.Self.ConfigRevision, Generation: rep.gen,
		PlatformURL: e.srv.URL, TokenFile: dirs.TokenFile,
		WorkspaceDir: dirs.Workspace, HomeDir: dirs.Home, RunnerDir: dirs.Runner, TmpDir: dirs.Tmp,
		Instructions: bundleText(t, "culture/culture.md") + "\n\n" + bundleText(t, "roles/representative.md"),
		Bootstrap:    boot, ToolCommand: conformance.BuildTools(t),
		Model:         chain.Endpoint("llm", modelID, api, nil),
		HarnessConfig: map[string]string{"claude_bin": bin, "codex_bin": bin, "pi_bin": bin},
		ExtraEnv: []string{"STEADMESH_PLATFORM_URL=" + e.srv.URL, "STEADMESH_TOKEN_FILE=" + dirs.TokenFile,
			"STEADMESH_RUNNER_DIR=" + dirs.Runner, fmt.Sprintf("STEADMESH_GENERATION=%d", rep.gen)},
	}
	var mu sync.Mutex
	var calls []string
	go func() {
		for ev := range a.Events() {
			if ev.Kind == harnesses.EventToolRequest {
				mu.Lock()
				calls = append(calls, string(ev.Data))
				mu.Unlock()
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if err := a.Prepare(ctx, env); err != nil {
		t.Fatal(err)
	}
	if _, err := a.StartOrResume(ctx, harnesses.RecoveryDescriptor{}); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
	for ok, _ := e.comm.Healthy(); !ok; ok, _ = e.comm.Healthy() {
		time.Sleep(20 * time.Millisecond)
	}

	automations := func() []map[string]any {
		var out []map[string]any
		for _, x := range rep.mustTool("automations.list", nil)["automations"].([]any) {
			out = append(out, x.(map[string]any))
		}
		return out
	}
	find := func(list []map[string]any, schedulePrefix string) map[string]any {
		for _, x := range list {
			if strings.HasPrefix(fmt.Sprint(x["schedule"]), schedulePrefix) {
				return x
			}
		}
		return nil
	}

	var failures []string
	replies, step := 0, 0
	say := func(text string, check func(list []map[string]any) string) {
		step++
		e.comm.Events <- connectors.InboundEvent{EventID: fmt.Sprintf("ev-%d", step), AccountID: orgfixture.Account,
			UserID: orgfixture.UserA, ChannelID: "D-ALICE", Text: text}
		if err := <-e.comm.Accepted(); err != nil {
			t.Fatal(err)
		}
		var d *runtimeapi.InboxDelivery
		for d == nil {
			d = rep.next()
		}
		mu.Lock()
		calls = nil
		mu.Unlock()
		sentBefore := len(e.comm.Sent())
		chain.SetExecution(d.ExecutionID)
		tr, err := a.Deliver(ctx, harnesses.Delivery{DeliveryID: d.DeliveryID, ExecutionID: d.ExecutionID, Attempt: d.Attempt,
			Message: d.Message, Passive: d.Passive, Timezone: boot.Self.Timezone})
		if err != nil || tr.Status != harnesses.TurnCompleted {
			t.Fatalf("step %d turn: %+v %v", step, tr, err)
		}
		rep.ack(d)
		for deadline := time.Now().Add(10 * time.Second); len(e.comm.Sent()) == sentBefore && time.Now().Before(deadline); {
			time.Sleep(50 * time.Millisecond)
		}
		mu.Lock()
		made := append([]string(nil), calls...)
		mu.Unlock()
		reply := "(no reply)"
		if sent := e.comm.Sent(); len(sent) > sentBefore {
			reply = sent[len(sent)-1].Text
		}
		list := automations()
		t.Logf("step %d: %q\n  tool calls: %s\n  reply: %s\n  final output (not delivered): %s\n  automations: %s", step, text,
			strings.Join(made, "\n              "), reply, strings.TrimSpace(tr.Output), summarise(list))
		if msg := check(list); msg != "" {
			failures = append(failures, fmt.Sprintf("step %d (%q): %s", step, text, msg))
		}
		if reply != "(no reply)" {
			replies++
		}
	}

	start := time.Now()
	say("Remind me in 20 minutes to call the dentist.", func(list []map[string]any) string {
		once := find(list, "once, at ")
		if len(list) != 1 || once == nil {
			return "want one one-off automation"
		}
		next := parseLocal(once["next_runs"].([]any)[0].(string))
		if d := next.Sub(start); d < 18*time.Minute || d > 22*time.Minute {
			return fmt.Sprintf("reminder runs %s from now, want about 20m", d.Round(time.Second))
		}
		return ""
	})
	say("Every weekday at 9am, check how the engineering team is getting on, and only tell me if something needs my attention.", func(list []map[string]any) string {
		w := find(list, "every weekday at 09:00 (Australia/Melbourne)")
		if len(list) != 2 || w == nil {
			return "want the reminder plus a weekday 09:00 check in Australia/Melbourne"
		}
		if in := strings.ToLower(fmt.Sprint(w["instruction"])); !strings.Contains(in, "attention") && !strings.Contains(in, "only") {
			return "the instruction does not say to stay quiet unless something needs attention: " + in
		}
		return ""
	})
	say("Actually, move that check to 10am.", func(list []map[string]any) string {
		if len(list) != 2 || find(list, "every weekday at 10:00 (Australia/Melbourne)") == nil {
			return "want the same check moved to weekdays 10:00, without a duplicate"
		}
		return ""
	})
	say("Pause the check for now.", func(list []map[string]any) string {
		if w := find(list, "every weekday at 10:00"); w == nil || w["status"] != "paused" {
			return "want the check paused"
		}
		return ""
	})
	say("OK, resume it.", func(list []map[string]any) string {
		if w := find(list, "every weekday at 10:00"); w == nil || w["status"] != "active" {
			return "want the check active again"
		}
		return ""
	})
	say("Cancel the dentist reminder we set up earlier.", func(list []map[string]any) string {
		if len(list) != 1 || find(list, "every weekday at 10:00") == nil {
			return "want only the weekday check left"
		}
		return ""
	})
	// Answering through messages.reply rather than final output is general
	// harness behaviour, not the automation interface: it is reported, not
	// failed.
	t.Logf("replies delivered with messages.reply: %d of %d turns", replies, step)
	for _, f := range failures {
		t.Error(f)
	}
}

func summarise(list []map[string]any) string {
	var parts []string
	for _, x := range list {
		parts = append(parts, fmt.Sprintf("[%v, %v, %v]", x["name"], x["schedule"], x["status"]))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}

func bundleText(t *testing.T, rel string) string {
	_, file, _, _ := runtime.Caller(0)
	b, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "bundles", rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// parseLocal reads recur.FormatLocal output, e.g. "Mon 2026-10-13 09:00 AEDT (+11:00)".
func parseLocal(s string) time.Time {
	f := strings.Fields(s)
	if len(f) < 5 {
		return time.Time{}
	}
	t, _ := time.Parse("2006-01-02 15:04 -07:00", f[1]+" "+f[2]+" "+strings.Trim(f[4], "()"))
	return t
}
