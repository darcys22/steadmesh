package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactWithholdsCredentialValues(t *testing.T) {
	in := json.RawMessage(`{"name":"connections.invoke","input":{"api_key":"sk-1","params":[{"Authorization":"Bearer x","title":"t"}]},"accessToken":"a"}`)
	out := string(redact(in))
	for _, secret := range []string{"sk-1", "Bearer x", `"a"`} {
		if strings.Contains(out, secret) {
			t.Fatalf("%s survived redaction: %s", secret, out)
		}
	}
	for _, kept := range []string{"connections.invoke", `"title":"t"`} {
		if !strings.Contains(out, kept) {
			t.Fatalf("%s was removed: %s", kept, out)
		}
	}
	plain := json.RawMessage(`{"content":"ok"}`)
	if got := redact(plain); string(got) != string(plain) {
		t.Fatalf("unchanged document rewritten: %s", got)
	}
	if got := redact(json.RawMessage(`not json`)); string(got) != "not json" {
		t.Fatalf("invalid JSON altered: %s", got)
	}
}
