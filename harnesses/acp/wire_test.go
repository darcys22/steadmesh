package acp

import (
	"encoding/json"
	"strings"
	"testing"
)

func decoder(t *testing.T) *TurnDecoder {
	t.Helper()
	d, err := NewTurnDecoder("session-1", "prompt-1")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

const chunk = `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello"}}}}`
const stop = `{"jsonrpc":"2.0","id":"prompt-1","result":{"stopReason":"end_turn"}}`

func TestMockPromptExchange(t *testing.T) {
	d := decoder(t)
	o, err := d.Decode([]byte(chunk))
	if err != nil || o.Kind != "output" || o.Text != "hello" {
		t.Fatalf("%+v %v", o, err)
	}
	o, err = d.Decode([]byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-1","update":{"sessionUpdate":"tool_call","toolCallId":"call-1","content":[{"type":"content","content":{"type":"text","text":"not executed"}}]}}}`))
	if err != nil || o.ToolCallID != "call-1" || o.Text != "" {
		t.Fatalf("%+v %v", o, err)
	}
	o, err = d.Decode([]byte(stop))
	if err != nil || o.StopReason != "end_turn" || d.Finish() != nil {
		t.Fatalf("%+v %v", o, err)
	}
	if _, err := d.Decode([]byte(chunk)); err == nil {
		t.Fatal("accepted event after stop")
	}
}

func TestPermissionsAlwaysCancelled(t *testing.T) {
	for _, id := range []string{`"permission-1"`, `9007199254740993`} {
		d := decoder(t)
		o, err := d.Decode([]byte(`{"jsonrpc":"2.0","id":` + id + `,"method":"session/request_permission","params":{"sessionId":"session-1","options":[{"optionId":"yes","kind":"allow_always"}]}}`))
		if err != nil || o.Kind != "permission_cancelled" {
			t.Fatalf("%+v %v", o, err)
		}
		var reply struct {
			ID     json.RawMessage `json:"id"`
			Result struct {
				Outcome struct {
					Outcome string `json:"outcome"`
				} `json:"outcome"`
			} `json:"result"`
		}
		if json.Unmarshal(o.Reply, &reply) != nil || string(reply.ID) != id || reply.Result.Outcome.Outcome != "cancelled" {
			t.Fatalf("unsafe reply: %s", o.Reply)
		}
		if strings.Contains(string(o.Reply), "selected") || strings.Contains(string(o.Reply), "allow") {
			t.Fatalf("permission granted: %s", o.Reply)
		}
	}
}

func TestInvalidOrUnsupportedRecordsPoisonTurn(t *testing.T) {
	cases := map[string]string{
		"malformed":                "not JSON",
		"duplicate ID":             `{"jsonrpc":"2.0","id":"other","id":"prompt-1","result":{"stopReason":"end_turn"}}`,
		"nested duplicate":         `{"jsonrpc":"2.0","id":"prompt-1","result":{"stopReason":"cancelled","stopReason":"end_turn"}}`,
		"trailing record":          stop + stop,
		"invalid UTF8":             strings.Replace(chunk, "hello", string([]byte{0xff}), 1),
		"empty":                    "",
		"null":                     "null",
		"batch":                    "[]",
		"version":                  strings.Replace(chunk, `"2.0"`, `"1.0"`, 1),
		"session":                  strings.Replace(chunk, "session-1", "session-2", 1),
		"id":                       strings.Replace(stop, "prompt-1", "other", 1),
		"numeric prompt ID":        strings.Replace(stop, `"prompt-1"`, `1`, 1),
		"missing stop":             `{"jsonrpc":"2.0","id":"prompt-1","result":{}}`,
		"unknown stop":             strings.Replace(stop, "end_turn", "success", 1),
		"remote error":             `{"jsonrpc":"2.0","id":"prompt-1","error":{"message":"secret-token"}}`,
		"mixed envelope":           `{"jsonrpc":"2.0","id":"x","method":"session/update","result":{},"params":{"sessionId":"session-1"}}`,
		"unknown request":          `{"jsonrpc":"2.0","id":"x","method":"fs/write_text_file","params":{"sessionId":"session-1","path":"/etc/test","content":"bad"}}`,
		"permission null ID":       `{"jsonrpc":"2.0","id":null,"method":"session/request_permission","params":{"sessionId":"session-1"}}`,
		"permission wrong session": `{"jsonrpc":"2.0","id":"x","method":"session/request_permission","params":{"sessionId":"other"}}`,
		"image":                    strings.Replace(chunk, `"type":"text"`, `"type":"image"`, 1),
		"unknown update":           strings.Replace(chunk, "agent_message_chunk", "unknown", 1),
		"oversized":                strings.Repeat("x", MaxRecordBytes+1),
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			d := decoder(t)
			o, err := d.Decode([]byte(line))
			if err == nil || len(o.Reply) != 0 {
				t.Fatalf("accepted invalid record: %+v", o)
			}
			if strings.Contains(err.Error(), "secret-token") {
				t.Fatal("leaked remote error payload")
			}
			if _, err = d.Decode([]byte(stop)); err == nil {
				t.Fatal("invalid record did not poison turn")
			}
			if d.Finish() == nil {
				t.Fatal("invalid turn completed")
			}
		})
	}
}

func TestEOFIsNotCompletion(t *testing.T) {
	d := decoder(t)
	_, _ = d.Decode([]byte(chunk))
	if d.Finish() == nil {
		t.Fatal("partial output claimed completion")
	}
	if _, err := d.Decode([]byte(stop)); err == nil {
		t.Fatal("resumed after EOF")
	}
}

func TestNonSuccessfulStopsRemainDistinct(t *testing.T) {
	for _, reason := range []string{"max_tokens", "max_turn_requests", "refusal", "cancelled"} {
		d := decoder(t)
		o, err := d.Decode([]byte(strings.Replace(stop, "end_turn", reason, 1)))
		if err != nil || o.StopReason != reason {
			t.Fatalf("%+v %v", o, err)
		}
	}
}
