package console

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"strings"
	"time"
)

var funcs = template.FuncMap{
	"dict":     dict,
	"list":     func(v ...string) []string { return v },
	"ago":      ago,
	"short":    short,
	"shortRef": shortRef,
	"duration": duration,
	"pretty":   pretty,
	"tone":     tone,
	"join":     strings.Join,
	"edgeID":   edgeID,
	"stamp":    func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	"deref":    func(t *time.Time) time.Time { return derefTime(t) },
	"activityLabel": func(kind string) string {
		if l, ok := activityLabels[kind]; ok {
			return l
		}
		return kind
	},
}

var activityLabels = map[string]string{
	"message":      "message",
	"run_started":  "run started",
	"run_finished": "run finished",
	"tool_request": "tool call",
	"tool_result":  "tool result",
	"error":        "error",
	"operation":    "connector",
	// model_request records the model and endpoint a request actually used.
	"model_request": "model request",
	"probe_result":  "readiness probe",
	// Sandbox access (docs/sandbox.html).
	"egress_denied":     "egress denied",
	"egress_revoked":    "connection closed: access revoked",
	"credential_issued": "credential delivered",
	"credential_denied": "credential refused",
}

// shortRef shortens the digest of an instruction reference
// (configmap:<name>/<key>#sha256:<digest>) for display; the full reference
// goes in the title.
func shortRef(ref string) string {
	base, digest, ok := strings.Cut(ref, "#sha256:")
	if !ok || len(digest) <= 12 {
		return ref
	}
	return base + "#sha256:" + digest[:12] + "…"
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// ago renders a time relative to now; it accepts time.Time or *time.Time.
func ago(now time.Time, v any) string {
	var t time.Time
	switch x := v.(type) {
	case time.Time:
		t = x
	case *time.Time:
		t = derefTime(x)
	}
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	switch {
	case d < 0:
		return "just now"
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return t.UTC().Format("2006-01-02 15:04")
}

// short abbreviates an id or revision digest for display.
func short(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func duration(start time.Time, end *time.Time, now time.Time) string {
	stop := now
	if end != nil {
		stop = *end
	}
	d := stop.Sub(start)
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return d.Round(time.Second).String()
}

// pretty indents a JSON document for display.
func pretty(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

// tone maps a state or status to a colour class.
func tone(s string) string {
	switch strings.ToLower(s) {
	case "working", "running", "succeeded", "completed", "done", "passed", "true", "bound", "sent", "ok", "published", "in_progress":
		return "ok"
	case "waiting", "queued", "starting", "pending", "leased", "unknown", "interrupted", "missing", "ready", "in_review":
		return "warn"
	case "blocked", "failed", "dead", "error", "false":
		return "bad"
	}
	return "muted"
}

// dict builds a map from alternating keys and values, for passing several
// values to a template.
func dict(kv ...any) (map[string]any, error) {
	if len(kv)%2 != 0 {
		return nil, fmt.Errorf("dict: odd number of arguments")
	}
	m := make(map[string]any, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict: key %v is not a string", kv[i])
		}
		m[k] = kv[i+1]
	}
	return m, nil
}
