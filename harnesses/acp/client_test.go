package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"net"
	"strings"
	"testing"
	"time"
)

type peer struct {
	t    *testing.T
	conn net.Conn
	in   *bufio.Reader
}

func (p peer) recv() map[string]json.RawMessage {
	p.t.Helper()
	_ = p.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := p.in.ReadBytes('\n')
	if err != nil {
		p.t.Error(err)
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(line, &m); err != nil {
		p.t.Error(err)
	}
	return m
}
func (p peer) send(v any) {
	p.t.Helper()
	_ = p.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	b, _ := json.Marshal(v)
	if _, err := p.conn.Write(append(b, '\n')); err != nil {
		p.t.Error(err)
	}
}
func (p peer) result(m map[string]json.RawMessage, result any) {
	p.send(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": result})
}
func (p peer) setup() {
	m := p.recv()
	if string(m["method"]) != `"initialize"` {
		p.t.Errorf("initialize: %s", m["method"])
	}
	if strings.Contains(string(m["params"]), `"terminal":true`) {
		p.t.Error("terminal advertised")
	}
	p.result(m, map[string]any{"protocolVersion": 1, "agentInfo": map[string]string{"name": CodexACPName, "version": CodexACPVersion}})
	m = p.recv()
	if string(m["method"]) != `"session/new"` {
		p.t.Errorf("new: %s", m["method"])
	}
	p.result(m, map[string]string{"sessionId": "s1"})
}
func connectPeer(t *testing.T, run func(peer)) (*Client, <-chan struct{}) {
	t.Helper()
	a, b := net.Pipe()
	c := NewClient(a)
	finished := make(chan struct{})
	t.Cleanup(func() { _ = c.Close(); _ = b.Close() })
	go func() { defer close(finished); run(peer{t, b, bufio.NewReader(b)}) }()
	return c, finished
}
func initSession(t *testing.T, c *Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Initialize(ctx, CodexACPName, CodexACPVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := c.NewSession(ctx, SessionParams{CWD: "/workspace", MCPServers: []StdioServer{}}); err != nil {
		t.Fatal(err)
	}
}
func TestClientWireLifecycle(t *testing.T) {
	c, finished := connectPeer(t, func(p peer) {
		p.setup()
		m := p.recv()
		p.send(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(`9007199254740993`), "method": "session/request_permission", "params": map[string]any{"sessionId": "s1", "options": []any{map[string]string{"kind": "allow_always", "optionId": "yes"}}}})
		reply := p.recv()
		if string(reply["id"]) != "9007199254740993" || !strings.Contains(string(reply["result"]), `"cancelled"`) {
			t.Errorf("unsafe permission response: %v", reply)
		}
		p.send(map[string]any{"jsonrpc": "2.0", "id": "file", "method": "fs/write_text_file", "params": map[string]string{"sessionId": "s1", "path": "/etc/example"}})
		reply = p.recv()
		if len(reply["error"]) == 0 || len(reply["result"]) != 0 {
			t.Errorf("unsupported client request accepted: %v", reply)
		}
		p.send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "hello"}}}})
		p.result(m, map[string]string{"stopReason": "end_turn"})
	})
	initSession(t, c)
	var output string
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	o, err := c.Prompt(ctx, "hello", func(o Observation) { output += o.Text })
	if err != nil || o.StopReason != "end_turn" || output != "hello" {
		t.Fatalf("%+v %q %v", o, output, err)
	}
	<-finished
}
func TestClientCancellationUnblocksWrite(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := NewClient(a)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.Initialize(ctx, CodexACPName, CodexACPVersion)
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("cancellation failed: %v", err)
	}
}
func TestClientRejectsWrongIdentity(t *testing.T) {
	c, done := connectPeer(t, func(p peer) {
		m := p.recv()
		p.result(m, map[string]any{"protocolVersion": 1, "agentInfo": map[string]string{"name": CodexACPName, "version": "unexpected"}})
	})
	if err := c.Initialize(context.Background(), CodexACPName, CodexACPVersion); err == nil {
		t.Fatal("accepted unverified version")
	}
	<-done
}
func TestClientWrongSessionPoisonsConnection(t *testing.T) {
	c, done := connectPeer(t, func(p peer) {
		p.setup()
		_ = p.recv()
		p.send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "other", "update": map[string]string{"sessionUpdate": "usage_update"}}})
	})
	initSession(t, c)
	if _, err := c.Prompt(context.Background(), "hello", nil); err == nil {
		t.Fatal("accepted cross-session update")
	}
	<-done
}
func TestClientPartialEOFIsNotCompletion(t *testing.T) {
	c, done := connectPeer(t, func(p peer) { p.setup(); _ = p.recv(); _ = p.conn.Close() })
	initSession(t, c)
	if _, err := c.Prompt(context.Background(), "hello", nil); err == nil {
		t.Fatal("EOF completed prompt")
	}
	<-done
}
func TestClientMalformedRecord(t *testing.T) {
	for _, record := range []string{`{"jsonrpc":"2.0","jsonrpc":"1.0"}`, strings.Repeat("x", MaxRecordBytes+1)} {
		t.Run(fmt.Sprint(len(record)), func(t *testing.T) {
			c, done := connectPeer(t, func(p peer) { _ = p.recv(); _, _ = p.conn.Write([]byte(record + "\n")) })
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := c.Initialize(ctx, CodexACPName, CodexACPVersion); err == nil {
				t.Fatal("accepted invalid record")
			}
			<-done
		})
	}
}

func TestSeatClientChecksSkillAuthorizationBeforePrompt(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p := peer{t, b, bufio.NewReader(b)}
		p.setup()
		_ = b.SetReadDeadline(time.Now().Add(time.Second))
		line, err := p.in.ReadBytes('\n')
		if err == nil {
			t.Errorf("prompt sent despite revoked authorization: %s", line)
		}
	}()
	env := planEnv()
	calls := 0
	env.LoadSkills = func(context.Context) (runtimeapi.SkillsBundleResponse, error) {
		calls++
		if calls == 1 {
			return runtimeapi.SkillsBundleResponse{}, nil
		}
		return runtimeapi.SkillsBundleResponse{}, errors.New("revoked")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := OpenCodexSession(ctx, a, env, "/opt/codex")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Prompt(ctx, "do not send", nil); err == nil {
		t.Fatal("authorization error ignored")
	}
	<-done
}
