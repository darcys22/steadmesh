// Package modelstub is a scripted model endpoint speaking the Anthropic
// Messages, OpenAI Responses and OpenAI Chat Completions APIs, streamed and
// not. Harness conformance tests run real harness binaries against it, and the
// e2e fakes serve it in the cluster.
//
// The reply depends on the conversation since the latest user text:
//   - each line "CALL <tool> <json>" in that text is one tool call, made in
//     order, one per model request (memory.write matches
//     mcp__steadmesh__memory_write, or the memory_write function in a
//     namespace);
//   - once every call has a result, it answers "done: <last tool result>";
//   - the readiness probe prompt makes it call the self tool;
//   - "SLOW" holds the stream open until the client goes away;
//   - otherwise it answers "hello from stub".
package modelstub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// API names as used by the platform (harnesses.API*).
const (
	AnthropicMessages = "anthropic_messages"
	OpenAIResponses   = "openai_responses"
	OpenAIChat        = "openai_chat"
)

// Request is one recorded request.
type Request struct {
	Method, Path string
	API          string
	Model        string
	// Key is the credential presented, from Authorization: Bearer or X-Api-Key.
	Key       string
	KeyHeader string
	Stream    bool
	Tools     []string
	LastUser  string
	// ToolResults counts tool results since LastUser; ToolResult is the last.
	ToolResults int
	ToolResult  string
	// ToolSearch means the client offers tool_search (Codex defers MCP tools
	// behind it); Searches counts searches since LastUser.
	ToolSearch bool `json:",omitempty"`
	Searches   int  `json:",omitempty"`
	Time       time.Time
}

// Stub is the scripted endpoint. Mount it at the API base root: it serves
// /v1/messages, /v1/responses, /v1/chat/completions and /v1/models.
type Stub struct {
	mu   sync.Mutex
	keys map[string]bool
	reqs []Request
	seq  int
}

// New returns a stub that accepts the given keys (none: any non-empty key).
func New(keys ...string) *Stub {
	s := &Stub{}
	s.SetKeys(keys...)
	return s
}

// SetKeys replaces the accepted keys.
func (s *Stub) SetKeys(keys ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = nil
	if len(keys) > 0 {
		s.keys = map[string]bool{}
		for _, k := range keys {
			s.keys[k] = true
		}
	}
}

// Requests returns the recorded requests.
func (s *Stub) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.reqs...)
}

// Reset clears the recorded requests.
func (s *Stub) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = nil
}

func (s *Stub) accept(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key == "" {
		return false
	}
	return s.keys == nil || s.keys[key]
}

func (s *Stub) record(r Request) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, r)
	s.seq++
	return s.seq
}

var callRe = regexp.MustCompile(`CALL ([A-Za-z0-9_.]+) (\{[^\n]*\})`)

// ProbeMarker identifies the platform's readiness probe prompt.
const ProbeMarker = "readiness probe"

// action is what the stub answers with.
type action struct {
	text      string
	tool      string // offered tool name to call
	namespace string // Responses namespace of the tool, if any
	search    string // tool_search query, when the tool is deferred
	args      string
	slow      bool
}

func decide(r Request, tools []offered) action {
	calls := callRe.FindAllStringSubmatch(r.LastUser, -1)
	if len(calls) == 0 && strings.Contains(r.LastUser, ProbeMarker) {
		calls = [][]string{{"", "self", "{}"}}
	}
	if r.ToolResults >= len(calls) && r.ToolResults > 0 {
		return action{text: "done: " + clip(r.ToolResult, 200)}
	}
	if strings.Contains(r.LastUser, "SLOW") {
		return action{slow: true}
	}
	if len(calls) == 0 {
		return action{text: "hello from stub"}
	}
	canonical, args := calls[r.ToolResults][1], calls[r.ToolResults][2]
	want := strings.ReplaceAll(canonical, ".", "_")
	for _, t := range tools {
		if t.name == want || strings.HasSuffix(t.name, "__"+want) {
			return action{tool: t.name, namespace: t.namespace, args: args}
		}
	}
	// A deferred tool is found with tool_search first, as a model would.
	if r.ToolSearch && r.Searches <= r.ToolResults {
		return action{search: strings.ReplaceAll(canonical, ".", " ") + " " + want}
	}
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.full()
	}
	return action{text: fmt.Sprintf("error: tool %s is not offered (tools: %s)", canonical, strings.Join(names, ", "))}
}

type offered struct{ namespace, name string }

func (o offered) full() string {
	if o.namespace != "" {
		return o.namespace + "/" + o.name
	}
	return o.name
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (s *Stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	key, header := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "authorization"
	if key == "" {
		key, header = r.Header.Get("X-Api-Key"), "x-api-key"
	}
	if !s.accept(key) {
		s.record(Request{Method: r.Method, Path: path, Key: key, KeyHeader: header, Time: time.Now()})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid api key"}}`)
		return
	}
	if r.Method == http.MethodGet && path == "/v1/models" {
		s.record(Request{Method: r.Method, Path: path, API: "models", Key: key, KeyHeader: header, Time: time.Now()})
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"stub-model","object":"model"}]}`)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	var parse func([]byte) (Request, []offered)
	var reply func(http.ResponseWriter, *http.Request, Request, action, int)
	switch path {
	case "/v1/messages":
		parse, reply = parseAnthropic, replyAnthropic
	case "/v1/messages/count_tokens":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":10}`)
		return
	case "/v1/responses":
		parse, reply = parseResponses, replyResponses
	case "/v1/chat/completions":
		parse, reply = parseChat, replyChat
	default:
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	req, tools := parse(body)
	req.Method, req.Path, req.Key, req.KeyHeader, req.Time = r.Method, path, key, header, time.Now()
	n := s.record(req)
	reply(w, r, req, decide(req, tools), n)
}

// ---- parsing ---------------------------------------------------------------

func textOf(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, b := range c {
			if m, ok := b.(map[string]any); ok {
				if tx, ok := m["text"].(string); ok {
					sb.WriteString(tx)
				}
			}
		}
		return sb.String()
	}
	return ""
}

func parseAnthropic(body []byte) (Request, []offered) {
	var in struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(body, &in)
	r := Request{API: AnthropicMessages, Model: in.Model, Stream: in.Stream}
	var tools []offered
	for _, t := range in.Tools {
		tools = append(tools, offered{name: t.Name})
		r.Tools = append(r.Tools, t.Name)
	}
	for i := len(in.Messages) - 1; i >= 0; i-- {
		msg := in.Messages[i]
		if msg.Role != "user" {
			continue
		}
		if t := textOf(msg.Content); t != "" {
			r.LastUser = t
			break
		}
		if arr, ok := msg.Content.([]any); ok {
			for _, b := range arr {
				if m, ok := b.(map[string]any); ok && m["type"] == "tool_result" {
					if r.ToolResults == 0 {
						r.ToolResult = textOf(m["content"])
					}
					r.ToolResults++
				}
			}
		}
	}
	return r, tools
}

func parseResponses(body []byte) (Request, []offered) {
	var in struct {
		Model  string            `json:"model"`
		Stream bool              `json:"stream"`
		Input  json.RawMessage   `json:"input"`
		Tools  []json.RawMessage `json:"tools"`
	}
	_ = json.Unmarshal(body, &in)
	r := Request{API: OpenAIResponses, Model: in.Model, Stream: in.Stream}
	var tools []offered
	addTools := func(list []json.RawMessage) {
		for _, raw := range list {
			var t struct {
				Type  string `json:"type"`
				Name  string `json:"name"`
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			}
			_ = json.Unmarshal(raw, &t)
			switch {
			case t.Type == "tool_search":
				r.ToolSearch = true
			case t.Type == "namespace":
				for _, nt := range t.Tools {
					tools = append(tools, offered{namespace: t.Name, name: nt.Name})
				}
			case t.Name != "":
				tools = append(tools, offered{name: t.Name})
			}
		}
	}
	addTools(in.Tools)
	var s string
	if json.Unmarshal(in.Input, &s) == nil {
		r.LastUser = s
		for _, t := range tools {
			r.Tools = append(r.Tools, t.full())
		}
		return r, tools
	}
	var items []map[string]any
	_ = json.Unmarshal(in.Input, &items)
	// Tools found by earlier searches are callable.
	var raws []json.RawMessage
	_ = json.Unmarshal(in.Input, &raws)
	for _, raw := range raws {
		var it struct {
			Type  string            `json:"type"`
			Tools []json.RawMessage `json:"tools"`
		}
		if json.Unmarshal(raw, &it) == nil && it.Type == "tool_search_output" {
			addTools(it.Tools)
		}
	}
	for _, t := range tools {
		r.Tools = append(r.Tools, t.full())
	}
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		if it["type"] == "tool_search_output" {
			r.Searches++
			continue
		}
		if it["type"] == "function_call_output" {
			if r.ToolResults == 0 {
				r.ToolResult = textOf(it["output"])
			}
			r.ToolResults++
			continue
		}
		if it["role"] == "user" && (it["type"] == nil || it["type"] == "message") {
			if t := textOf(it["content"]); t != "" {
				r.LastUser = t
				break
			}
		}
	}
	return r, tools
}

func parseChat(body []byte) (Request, []offered) {
	var in struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(body, &in)
	r := Request{API: OpenAIChat, Model: in.Model, Stream: in.Stream}
	var tools []offered
	for _, t := range in.Tools {
		tools = append(tools, offered{name: t.Function.Name})
		r.Tools = append(r.Tools, t.Function.Name)
	}
	for i := len(in.Messages) - 1; i >= 0; i-- {
		if in.Messages[i].Role == "tool" {
			if r.ToolResults == 0 {
				r.ToolResult = textOf(in.Messages[i].Content)
			}
			r.ToolResults++
			continue
		}
		if in.Messages[i].Role == "user" {
			if t := textOf(in.Messages[i].Content); t != "" {
				r.LastUser = t
				break
			}
		}
	}
	return r, tools
}

// ---- replies ---------------------------------------------------------------

type sse struct {
	w http.ResponseWriter
	f http.Flusher
}

func newSSE(w http.ResponseWriter) *sse {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	f, _ := w.(http.Flusher)
	return &sse{w: w, f: f}
}

func (s *sse) event(name string, v any) {
	b, _ := json.Marshal(v)
	if name != "" {
		fmt.Fprintf(s.w, "event: %s\n", name)
	}
	fmt.Fprintf(s.w, "data: %s\n\n", b)
	if s.f != nil {
		s.f.Flush()
	}
}

func (s *sse) raw(line string) {
	fmt.Fprint(s.w, line)
	if s.f != nil {
		s.f.Flush()
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func replyAnthropic(w http.ResponseWriter, r *http.Request, req Request, a action, n int) {
	id := fmt.Sprintf("msg_stub_%d", n)
	usage := map[string]int{"input_tokens": 12, "output_tokens": 3}
	var block map[string]any
	stop := "end_turn"
	if a.tool != "" {
		block, stop = map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_stub_%d", n), "name": a.tool, "input": json.RawMessage(a.args)}, "tool_use"
	} else {
		block = map[string]any{"type": "text", "text": a.text}
	}
	if !req.Stream {
		writeJSON(w, map[string]any{"id": id, "type": "message", "role": "assistant", "model": req.Model,
			"content": []any{block}, "stop_reason": stop, "stop_sequence": nil, "usage": usage})
		return
	}
	s := newSSE(w)
	s.event("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": id, "type": "message", "role": "assistant",
		"model": req.Model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 12, "output_tokens": 1}}})
	if a.slow {
		<-r.Context().Done()
		return
	}
	if a.tool != "" {
		s.event("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "tool_use", "id": block["id"], "name": a.tool, "input": map[string]any{}}})
		s.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": a.args}})
	} else {
		s.event("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		s.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": a.text}})
	}
	s.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	s.event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 3}})
	s.event("message_stop", map[string]any{"type": "message_stop"})
}

func replyResponses(w http.ResponseWriter, r *http.Request, req Request, a action, n int) {
	id := fmt.Sprintf("resp_stub_%d", n)
	var item map[string]any
	if a.search != "" {
		item = map[string]any{"type": "tool_search_call", "id": fmt.Sprintf("ts_stub_%d", n), "call_id": fmt.Sprintf("search_stub_%d", n),
			"execution": "client", "status": "completed", "arguments": map[string]any{"query": a.search, "limit": 20}}
		done := map[string]any{"id": id, "object": "response", "status": "completed", "model": req.Model, "output": []any{item},
			"usage": map[string]any{"input_tokens": 12, "output_tokens": 3, "total_tokens": 15}}
		if !req.Stream {
			writeJSON(w, done)
			return
		}
		s := newSSE(w)
		s.event("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": id, "object": "response", "status": "in_progress", "model": req.Model}})
		s.event("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
		s.event("response.completed", map[string]any{"type": "response.completed", "response": done})
		return
	}
	if a.tool != "" {
		item = map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_stub_%d", n), "call_id": fmt.Sprintf("call_stub_%d", n),
			"name": a.tool, "arguments": a.args, "status": "completed"}
		if a.namespace != "" {
			item["namespace"] = a.namespace
		}
	} else {
		item = map[string]any{"type": "message", "id": fmt.Sprintf("msg_stub_%d", n), "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": a.text, "annotations": []any{}}}}
	}
	usage := map[string]any{"input_tokens": 12, "output_tokens": 3, "total_tokens": 15}
	done := map[string]any{"id": id, "object": "response", "status": "completed", "model": req.Model, "output": []any{item}, "usage": usage}
	if !req.Stream {
		writeJSON(w, done)
		return
	}
	s := newSSE(w)
	s.event("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": id, "object": "response", "status": "in_progress", "model": req.Model}})
	if a.slow {
		<-r.Context().Done()
		return
	}
	added := map[string]any{}
	for k, v := range item {
		added[k] = v
	}
	if a.tool != "" {
		added["arguments"], added["status"] = "", "in_progress"
		s.event("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": added})
		s.event("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": item["id"], "delta": a.args})
		s.event("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "output_index": 0, "item_id": item["id"], "arguments": a.args})
	} else {
		added["content"], added["status"] = []any{}, "in_progress"
		s.event("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": added})
		s.event("response.content_part.added", map[string]any{"type": "response.content_part.added", "output_index": 0, "item_id": item["id"], "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
		s.event("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": 0, "item_id": item["id"], "content_index": 0, "delta": a.text})
		s.event("response.output_text.done", map[string]any{"type": "response.output_text.done", "output_index": 0, "item_id": item["id"], "content_index": 0, "text": a.text})
	}
	s.event("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
	s.event("response.completed", map[string]any{"type": "response.completed", "response": done})
}

func replyChat(w http.ResponseWriter, r *http.Request, req Request, a action, n int) {
	id := fmt.Sprintf("chatcmpl-stub-%d", n)
	created := time.Now().Unix()
	usage := map[string]int{"prompt_tokens": 12, "completion_tokens": 3, "total_tokens": 15}
	msg := map[string]any{"role": "assistant", "content": a.text}
	finish := "stop"
	var call map[string]any
	if a.tool != "" {
		call = map[string]any{"id": fmt.Sprintf("call_stub_%d", n), "type": "function", "function": map[string]any{"name": a.tool, "arguments": a.args}}
		msg, finish = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{call}}, "tool_calls"
	}
	if !req.Stream {
		writeJSON(w, map[string]any{"id": id, "object": "chat.completion", "created": created, "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}}, "usage": usage})
		return
	}
	s := newSSE(w)
	chunk := func(delta map[string]any, finish any) {
		s.event("", map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
	}
	chunk(map[string]any{"role": "assistant", "content": ""}, nil)
	if a.slow {
		<-r.Context().Done()
		return
	}
	if call != nil {
		call["index"] = 0
		chunk(map[string]any{"tool_calls": []any{call}}, nil)
	} else {
		chunk(map[string]any{"content": a.text}, nil)
	}
	chunk(map[string]any{}, finish)
	s.event("", map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model, "choices": []any{}, "usage": usage})
	s.raw("data: [DONE]\n\n")
}
