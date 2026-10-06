//go:build live

package live

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const orgNS = "steadmesh-example"

var root, _ = filepath.Abs("../..")

func TestLive(t *testing.T) {
	need := []string{"SLACK_BOT_TOKEN", "SLACK_APP_TOKEN", "SLACK_TEAM_ID", "SLACK_USER_SEAN", "SLACK_USER_ALEX",
		"SLACK_USER_TOKEN_SEAN", "SLACK_USER_TOKEN_ALEX", "LINEAR_API_KEY", "LINEAR_TEAM_ID", "ANTHROPIC_API_KEY"}
	for _, k := range need {
		if os.Getenv(k) == "" {
			t.Skipf("live test needs %s (see tests/live/README.md)", k)
		}
	}
	kctx := os.Getenv("KIND_CONTEXT")
	if !strings.HasPrefix(kctx, "kind-") {
		t.Fatalf("refusing to run against context %q", kctx)
	}
	humans := fmt.Sprintf(`{sean={slack_user_id=%q},alex={slack_user_id=%q}}`, os.Getenv("SLACK_USER_SEAN"), os.Getenv("SLACK_USER_ALEX"))
	env := []string{
		"TF_VAR_harness=claude-code",
		"TF_VAR_slack_bot_token=" + os.Getenv("SLACK_BOT_TOKEN"),
		"TF_VAR_slack_app_token=" + os.Getenv("SLACK_APP_TOKEN"),
		"TF_VAR_linear_api_key=" + os.Getenv("LINEAR_API_KEY"),
		"TF_VAR_anthropic_api_key=" + os.Getenv("ANTHROPIC_API_KEY"),
		"TF_VAR_slack_workspace_id=" + os.Getenv("SLACK_TEAM_ID"),
		"TF_VAR_linear_team_id=" + os.Getenv("LINEAR_TEAM_ID"),
		"TF_VAR_humans=" + humans,
	}
	mustRun(t, env, "make", "-C", "examples", "apply")

	sean := &human{token: os.Getenv("SLACK_USER_TOKEN_SEAN"), botToken: os.Getenv("SLACK_BOT_TOKEN")}
	alex := &human{token: os.Getenv("SLACK_USER_TOKEN_ALEX"), botToken: os.Getenv("SLACK_BOT_TOKEN")}

	t.Run("A04 A05 each human reaches their own representative", func(t *testing.T) {
		sean.say(t, "Hi! Please remember that I prefer very short status updates. Reply with a one-line acknowledgement.")
		alex.say(t, "Hello, who are you and who do you represent? One sentence please.")
		if r := sean.awaitReply(t, 5*time.Minute); r == "" {
			t.Fatal("no reply to sean")
		}
		if r := alex.awaitReply(t, 5*time.Minute); r == "" {
			t.Fatal("no reply to alex")
		}
	})

	project := fmt.Sprintf("Live acceptance %d", time.Now().Unix())
	t.Run("A06 A21 delegation creates a tracker project", func(t *testing.T) {
		sean.say(t, fmt.Sprintf("Please ask the engineering lead to create a Linear project named exactly %q for a small internal onboarding checklist, and tell me when it exists.", project))
		deadline := time.Now().Add(15 * time.Minute)
		for !linearProjectExists(t, project) {
			if time.Now().After(deadline) {
				t.Fatalf("project %q not created in Linear", project)
			}
			time.Sleep(15 * time.Second)
		}
		if r := sean.awaitReply(t, 10*time.Minute); r == "" {
			t.Fatal("representative did not follow up")
		}
	})

	t.Run("A07 A08 memory survives a pod kill", func(t *testing.T) {
		name := strings.TrimSpace(mustRun(t, nil, "kubectl", "--context", kctx, "-n", orgNS, "get", "agentseats",
			"-l", "steadmesh.io/seat=representative_sean", "-o", "jsonpath={.items[0].metadata.name}"))
		mustRun(t, nil, "kubectl", "--context", kctx, "-n", orgNS, "delete", "pod", name+"-0")
		sean.say(t, "What format do I prefer for status updates? Answer in a few words.")
		r := sean.awaitReply(t, 6*time.Minute)
		if !strings.Contains(strings.ToLower(r), "short") && !strings.Contains(strings.ToLower(r), "brief") {
			t.Fatalf("representative did not recall the preference: %q", r)
		}
	})
}

// human sends DMs to the bot as a test user and reads the bot's replies.
type human struct {
	token, botToken string
	channel         string
	lastTS          string
}

func (h *human) api(t *testing.T, token, method string, form url.Values) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("POST", "https://slack.com/api/"+method, strings.NewReader(form.Encode()))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["ok"] != true {
		t.Fatalf("slack %s: %v", method, out["error"])
	}
	return out
}

func (h *human) say(t *testing.T, text string) {
	t.Helper()
	if h.channel == "" {
		bot := h.api(t, h.botToken, "auth.test", nil)["user_id"].(string)
		ch := h.api(t, h.token, "conversations.open", url.Values{"users": {bot}})["channel"].(map[string]any)
		h.channel = ch["id"].(string)
	}
	h.lastTS = h.api(t, h.token, "chat.postMessage", url.Values{"channel": {h.channel}, "text": {text}})["ts"].(string)
}

// awaitReply returns the newest bot message posted after the last message sent.
func (h *human) awaitReply(t *testing.T, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		hist := h.api(t, h.token, "conversations.history", url.Values{"channel": {h.channel}, "oldest": {h.lastTS}})
		for _, m := range hist["messages"].([]any) {
			msg := m.(map[string]any)
			if _, isBot := msg["bot_id"]; isBot {
				h.lastTS = msg["ts"].(string)
				return msg["text"].(string)
			}
		}
		time.Sleep(5 * time.Second)
	}
	return ""
}

func linearProjectExists(t *testing.T, name string) bool {
	q := map[string]any{"query": `query($n:String!){projects(filter:{name:{eq:$n}}){nodes{id}}}`, "variables": map[string]any{"n": name}}
	b, _ := json.Marshal(q)
	req, _ := http.NewRequest("POST", "https://api.linear.app/graphql", bytes.NewReader(b))
	req.Header.Set("Authorization", os.Getenv("LINEAR_API_KEY"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("linear: %v", err)
		return false
	}
	defer resp.Body.Close()
	var out struct {
		Data struct {
			Projects struct{ Nodes []struct{ ID string } } `json:"projects"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return len(out.Data.Projects.Nodes) > 0
}

func mustRun(t *testing.T, env []string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}
