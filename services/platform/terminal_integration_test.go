//go:build integration

package platform_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/connectors/terminal"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/internal/fakeconn"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/darcys22/steadmesh/services/platform"
)

// TestTerminalChannelRoundTrip chats with a representative through the
// terminal adapter mounted on the platform API: a typed line reaches the
// inbox, and replies and proactive messages stream back to the human.
func TestTerminalChannelRoundTrip(t *testing.T) {
	secrets := &fakeconn.Secrets{}
	secrets.Set("k8s:terminal", map[string]string{"carol": "tok-carol"})
	e := newEnvWith(t, false, func(o *platform.Options) {
		o.Secrets = secrets
		o.Factories.Communication["terminal"] = terminal.New
	})
	sp := orgfixture.Spec()
	sp.Connections["terminal"] = spec.Connection{Adapter: "terminal", SecretRef: "k8s:terminal"}
	sp.MemoryStores["rep_c"] = spec.MemoryStore{}
	rep := sp.Seats["rep_a"]
	rep.PersonalMemory = "rep_c"
	sp.Seats["rep_c"] = rep
	sp.ChannelBindings["carol"] = spec.ChannelBinding{Connection: "terminal", ExternalUserID: "carol", Seat: "rep_c", Mode: "direct_message"}
	e.sync(sp)
	repC := e.seat("rep_c")
	base := runtimeapi.PathChannels + e.org + "/terminal"

	if code, _ := e.request("POST", base+"/messages", "wrong", nil, terminal.Inbound{ID: "1", Text: "hi"}); code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", code)
	}
	if code, _ := e.request("GET", runtimeapi.PathChannels+e.org+"/slack/events", "tok-carol", nil, nil); code != http.StatusNotFound {
		t.Fatalf("slack is not a dial-in channel: %d", code)
	}
	if code, _ := e.request("GET", runtimeapi.PathChannels+e.org+"/missing/events", "tok-carol", nil, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("undeclared connection: %d", code)
	}

	// The ingress loop starts asynchronously after sync.
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, b := e.request("POST", base+"/messages", "tok-carol", nil, terminal.Inbound{ID: "1", Text: "what's on today?"})
		if code == http.StatusAccepted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("post: %d %s", code, b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	d := repC.next()
	if d == nil || d.Message.Origin != "human" || d.Message.Binding != "carol" || !strings.Contains(d.Message.Body, "what's on today?") {
		t.Fatalf("human delivery = %+v", d)
	}
	repC.ack(d)

	// Sent while the terminal is closed, so it waits for the stream.
	repC.mustTool("messages.reply", map[string]any{"message_id": d.Message.MessageID, "body": "Two reviews and a deploy."})
	time.Sleep(300 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+base+"/events", nil)
	req.Header.Set("Authorization", "Bearer tok-carol")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("events: %v %v", resp, err)
	}
	defer resp.Body.Close()
	events := make(chan terminal.Event, 4)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var ev terminal.Event
				if json.Unmarshal([]byte(data), &ev) == nil {
					events <- ev
				}
			}
		}
	}()
	next := func() string {
		select {
		case ev := <-events:
			return ev.Text
		case <-time.After(5 * time.Second):
			t.Fatal("no message from the representative")
			return ""
		}
	}
	if got := next(); got != "Two reviews and a deploy." {
		t.Fatalf("reply = %q", got)
	}
	repC.mustTool("messages.reply", map[string]any{"binding": "carol", "body": "Deploy finished."})
	if got := next(); got != "Deploy finished." {
		t.Fatalf("proactive = %q", got)
	}
}
