package main

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/connectors/terminal"
)

// echoSink answers every message from the terminal with a markdown reply.
type echoSink struct{ a connectors.Communication }

func (s echoSink) Accept(ctx context.Context, _ string, ev connectors.InboundEvent) error {
	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = s.a.Send(context.Background(), connectors.OutboundMessage{ExternalUserID: ev.UserID,
			Text: "Got it: **" + ev.Text + "**\n\n- first\n- second", IdempotencyKey: ev.EventID})
	}()
	return nil
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ansi.Strip(s.b.String())
}

func TestChatTUIRoundTrip(t *testing.T) {
	a, err := terminal.New(connectors.Config{Key: "terminal", Secret: map[string]string{"sean": "tok"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = a.Run(ctx, echoSink{a}) }()
	srv := httptest.NewServer(a.(connectors.HTTPIngress).Handler())
	defer srv.Close()

	in, typed := io.Pipe()
	out := &syncBuffer{}
	m := newChatModel(ctx, &chatClient{base: srv.URL, token: "tok"}, "sean", "terminal", true)
	done := make(chan error, 1)
	go func() { done <- runChatTUI(ctx, m, tea.WithInput(in), tea.WithOutput(out), tea.WithWindowSize(80, 24)) }()

	waitOutput := func(want string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if strings.Contains(out.String(), want) {
				return
			}
		}
		t.Fatalf("output never contained %q:\n%s", want, out.String())
	}
	waitOutput("Chatting with sean's representative over terminal.")
	_, _ = io.WriteString(typed, "hello there\r")
	waitOutput("you ")
	waitOutput("Got it: hello there")
	got := out.String()
	if strings.Contains(got, "**") {
		t.Fatalf("markdown not rendered:\n%s", got)
	}
	if !strings.Contains(got, "• first") {
		t.Fatalf("list not rendered:\n%s", got)
	}
	_, _ = io.WriteString(typed, "\x03") // ctrl+c
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ctrl+c did not quit")
	}
	t.Log("\n" + got)
}

func TestChatHistoryRecall(t *testing.T) {
	m := newChatModel(context.Background(), &chatClient{}, "sean", "terminal", true)
	m.history = []string{"first", "second"}
	m.recall = len(m.history)
	m.input.SetValue("draft")
	for _, step := range []struct {
		delta int
		want  string
		moved bool
	}{{-1, "second", true}, {-1, "first", true}, {-1, "first", false}, {1, "second", true}, {1, "draft", true}, {1, "draft", false}} {
		if moved := m.browse(step.delta); moved != step.moved || m.input.Value() != step.want {
			t.Fatalf("browse(%d) = %v, input %q; want %v, %q", step.delta, moved, m.input.Value(), step.moved, step.want)
		}
	}
}
