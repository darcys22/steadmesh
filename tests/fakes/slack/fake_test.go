package fakeslack

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func postJSON(t *testing.T, u string, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(u, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func api(t *testing.T, base, method, token string, form url.Values) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/api/"+method, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func dial(t *testing.T, base string) *websocket.Conn {
	t.Helper()
	open := api(t, base, "apps.connections.open", "xapp-1", nil)
	wsURL, _ := open["url"].(string)
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	var hello map[string]any
	if err := c.ReadJSON(&hello); err != nil || hello["type"] != "hello" {
		t.Fatalf("hello: %v %v", hello, err)
	}
	return c
}

type envelopeMsg struct {
	EnvelopeID   string          `json:"envelope_id"`
	Type         string          `json:"type"`
	RetryAttempt int             `json:"retry_attempt"`
	RetryReason  string          `json:"retry_reason"`
	Payload      json.RawMessage `json:"payload"`
}

func TestWebAPI(t *testing.T) {
	f := New(Options{})
	defer f.Close()
	srv := httptest.NewServer(f)
	defer srv.Close()

	if r := api(t, srv.URL, "auth.test", "xoxb-1", nil); r["ok"] != true || r["team_id"] != "T0FAKE" {
		t.Fatalf("auth.test %v", r)
	}
	if r := api(t, srv.URL, "auth.test", "bogus", nil); r["error"] != "invalid_auth" {
		t.Fatalf("bad token %v", r)
	}
	if r := api(t, srv.URL, "apps.connections.open", "xoxb-1", nil); r["error"] != "invalid_auth" {
		t.Fatalf("bot token must not open sockets %v", r)
	}
	if r := api(t, srv.URL, "users.info", "xoxb-1", url.Values{"user": {"U0BOB"}}); r["ok"] != true {
		t.Fatalf("users.info %v", r)
	}
	if r := api(t, srv.URL, "conversations.open", "xoxb-1", url.Values{"users": {"U0ALICE"}}); r["channel"].(map[string]any)["id"] != "D0ALICE" {
		t.Fatalf("conversations.open %v", r)
	}
	r := api(t, srv.URL, "chat.postMessage", "xoxb-1", url.Values{"channel": {"D0ALICE"}, "text": {"hi"}})
	if r["ok"] != true || r["ts"] == "" {
		t.Fatalf("postMessage %v", r)
	}

	resp, err := http.Get(srv.URL + "/_test/posted")
	if err != nil {
		t.Fatal(err)
	}
	var posted []PostedMessage
	_ = json.NewDecoder(resp.Body).Decode(&posted)
	resp.Body.Close()
	if len(posted) != 1 || posted[0].Text != "hi" {
		t.Fatalf("posted %+v", posted)
	}
}

func TestFailModes(t *testing.T) {
	f := New(Options{})
	defer f.Close()
	srv := httptest.NewServer(f)
	defer srv.Close()

	if resp := postJSON(t, srv.URL+"/_test/fail", `{"method":"chat.postMessage"}`); resp.StatusCode != 400 {
		t.Fatalf("missing mode accepted: %d", resp.StatusCode)
	}
	postJSON(t, srv.URL+"/_test/fail", `{"method":"chat.postMessage","mode":"rate_limited","count":1}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/chat.postMessage",
		strings.NewReader(url.Values{"channel": {"D0A"}, "text": {"x"}, "token": {"xoxb-1"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("rate limited: %d", resp.StatusCode)
	}

	f.Fail("chat.postMessage", "ambiguous", 1)
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/api/chat.postMessage",
		strings.NewReader(url.Values{"channel": {"D0A"}, "text": {"lost"}, "token": {"xoxb-1"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("ambiguous mode should drop the connection")
	}
	if p := f.Posted(); len(p) != 1 || p[0].Text != "lost" {
		t.Fatalf("ambiguous must commit: %+v", p)
	}
}

func TestSocketModeRedeliveryAndAcks(t *testing.T) {
	f := New(Options{RedeliveryTimeout: 100 * time.Millisecond, MaxRetries: 2})
	defer f.Close()
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := dial(t, srv.URL)

	resp := postJSON(t, srv.URL+"/_test/dm", `{"user":"U0ALICE","text":"hello","event_id":"Ev1"}`)
	var inj Injected
	_ = json.NewDecoder(resp.Body).Decode(&inj)
	resp.Body.Close()
	if inj.EventID != "Ev1" || inj.Channel != "D0ALICE" {
		t.Fatalf("injected %+v", inj)
	}

	// First delivery, then a redelivery with retry_attempt 1 because we did not ack.
	var first, second envelopeMsg
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := c.ReadJSON(&first); err != nil {
		t.Fatal(err)
	}
	if err := c.ReadJSON(&second); err != nil {
		t.Fatal(err)
	}
	if first.Type != "events_api" || first.RetryAttempt != 0 || second.RetryAttempt != 1 || second.RetryReason != "timeout" {
		t.Fatalf("first %+v second %+v", first, second)
	}
	var payload struct {
		TeamID  string `json:"team_id"`
		EventID string `json:"event_id"`
		Event   struct {
			Type, User, Text, Channel string
			ChannelType               string `json:"channel_type"`
		} `json:"event"`
	}
	_ = json.Unmarshal(first.Payload, &payload)
	if payload.TeamID != "T0FAKE" || payload.EventID != "Ev1" || payload.Event.User != "U0ALICE" ||
		payload.Event.ChannelType != "im" || payload.Event.Text != "hello" {
		t.Fatalf("payload %s", first.Payload)
	}

	if err := c.WriteJSON(map[string]string{"envelope_id": second.EnvelopeID}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(f.Acks()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	acks := f.Acks()
	if len(acks) != 1 || acks[0].EventID != "Ev1" || acks[0].RetryAttempt != 1 {
		t.Fatalf("acks %+v", acks)
	}
	if st := f.Envelopes(); len(st) != 1 || !st[0].Acked || st[0].Attempts != 2 {
		t.Fatalf("envelopes %+v", st)
	}
}

func TestSocketModeGivesUpAfterMaxRetries(t *testing.T) {
	f := New(Options{RedeliveryTimeout: 30 * time.Millisecond, MaxRetries: 1})
	defer f.Close()
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := dial(t, srv.URL)
	go func() {
		for {
			var m envelopeMsg
			if err := c.ReadJSON(&m); err != nil {
				return
			}
		}
	}()
	_, _ = f.InjectDM(DM{User: "U0ALICE", Text: "never acked"})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st := f.Envelopes(); st[0].GaveUp {
			if st[0].Attempts != 2 {
				t.Fatalf("attempts %d", st[0].Attempts)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("never gave up")
}

func TestEnvelopesWaitForConnection(t *testing.T) {
	f := New(Options{})
	defer f.Close()
	srv := httptest.NewServer(f)
	defer srv.Close()
	_, _ = f.InjectDM(DM{User: "U0BOB", Text: "queued"})
	time.Sleep(100 * time.Millisecond)
	if st := f.Envelopes(); st[0].Attempts != 0 {
		t.Fatalf("delivered without a connection: %+v", st)
	}
	c := dial(t, srv.URL)
	var m envelopeMsg
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := c.ReadJSON(&m); err != nil || m.Type != "events_api" {
		t.Fatalf("%+v %v", m, err)
	}
}

func TestTokenRotation(t *testing.T) {
	f := New(Options{})
	defer f.Close()
	srv := httptest.NewServer(f)
	defer srv.Close()
	postJSON(t, srv.URL+"/_test/tokens", `{"bot":["xoxb-old","xoxb-new"]}`).Body.Close()
	for _, tok := range []string{"xoxb-old", "xoxb-new"} {
		if r := api(t, srv.URL, "auth.test", tok, nil); r["ok"] != true {
			t.Fatalf("%s during rotation: %v", tok, r)
		}
	}
	if r := api(t, srv.URL, "auth.test", "xoxb-other", nil); r["error"] != "invalid_auth" {
		t.Fatalf("unlisted token accepted: %v", r)
	}
	postJSON(t, srv.URL+"/_test/tokens", `{"bot":["xoxb-new"]}`).Body.Close()
	if r := api(t, srv.URL, "auth.test", "xoxb-old", nil); r["error"] != "invalid_auth" {
		t.Fatalf("revoked token accepted: %v", r)
	}
	postJSON(t, srv.URL+"/_test/tokens", `{}`).Body.Close()
	if r := api(t, srv.URL, "auth.test", "xoxb-any", nil); r["ok"] != true {
		t.Fatalf("default rule not restored: %v", r)
	}
}
