package api

import (
	"net/http/httptest"
	"testing"
)

func TestMeterUsage(t *testing.T) {
	for _, tc := range []struct {
		name, contentType string
		chunks            []string
		want              map[string]int64
	}{
		{"anthropic stream", "text/event-stream", []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":1}}}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_",
			"tokens\":30}}\n\n",
		}, map[string]int64{"input_tokens": 12, "output_tokens": 30}},
		{"responses stream", "text/event-stream", []string{
			"event: response.created\ndata: {\"response\":{\"id\":\"r\"}}\n\n",
			"event: response.completed\ndata: {\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":7,\"total_tokens\":12}}}\n\n",
		}, map[string]int64{"input_tokens": 5, "output_tokens": 7, "total_tokens": 12}},
		{"chat stream", "text/event-stream; charset=utf-8", []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4}}\n\ndata: [DONE]\n\n",
		}, map[string]int64{"prompt_tokens": 3, "completion_tokens": 4}},
		{"json", "application/json", []string{`{"id":"x","usage":{"input_tokens":2,`, `"output_tokens":9}}`},
			map[string]int64{"input_tokens": 2, "output_tokens": 9}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			m := &meter{ResponseWriter: rec}
			m.Header().Set("Content-Type", tc.contentType)
			m.WriteHeader(200)
			for _, c := range tc.chunks {
				_, _ = m.Write([]byte(c))
			}
			m.done()
			if m.status != 200 || len(m.usage) != len(tc.want) {
				t.Fatalf("status %d usage %v, want %v", m.status, m.usage, tc.want)
			}
			for k, v := range tc.want {
				if m.usage[k] != v {
					t.Errorf("%s = %d, want %d", k, m.usage[k], v)
				}
			}
		})
	}
}
