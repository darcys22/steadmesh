package runtimeapi

import (
	"encoding/json"
	"strings"
	"testing"
)

var resultSchema = json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"},"findings":{"type":"array","maxItems":2,"items":{"type":"string","maxLength":20}}},"required":["ok","findings"],"additionalProperties":false}`)

func TestStructuredOutput(t *testing.T) {
	valid, err := ValidateStructuredOutput(resultSchema, []byte(` {"ok":false,"findings":["problem"]} `))
	if err != nil || string(valid) != `{"ok":false,"findings":["problem"]}` {
		t.Fatalf("%s: %v", valid, err)
	}
	// A schema-valid negative answer is a valid technical result.
	for _, raw := range []string{
		`{"ok":true}`, `{"ok":"true","findings":[]}`, `{"ok":true,"findings":[],"extra":1}`,
		`{"ok":true,"findings":["a","b","c"]}`, `{"ok":true,"ok":false,"findings":[]}`,
		"```json\n{}\n```", `{"ok":true,"findings":[]} {}`, `null`,
	} {
		if _, err := ValidateStructuredOutput(resultSchema, []byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}

func TestSchemaFailClosedAndBounded(t *testing.T) {
	if err := ValidateOutputSchema(nil); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`null`, `true`, `{}`, `{"type":"object","$ref":"https://example.com/schema"}`,
		`{"type":"object","$ref":"file:///etc/passwd"}`, `{"type":"object","properties":{"x":{"type":"string","pattern":".*"}}}`,
		`{"type":"string","type":"number"}`, `{"type":"object","additionalProperties":{"type":"string"}}`,
		`{"type":["null","string"]}`, `{"type":"array","items":[]}`, `{"type":"string","minLength":-1}`,
		`{"type":"string","enum":[{}]}`, `{"type":"string","enum":["x","x"]}`, `{"type":"number","minimum":1e99999}`,
	} {
		if err := ValidateOutputSchema([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	if err := ValidateOutputSchema([]byte(strings.Repeat(" ", MaxOutputSchemaBytes) + `{}`)); err == nil {
		t.Fatal("oversize")
	}
	if _, err := decodeBoundedJSON([]byte(strings.Repeat("[", 18)+"0"+strings.Repeat("]", 18)), 1024); err == nil {
		t.Fatal("depth")
	}
	if _, err := decodeBoundedJSON([]byte(`{"a":1,"a":2}`), 1024); err == nil {
		t.Fatal("duplicates")
	}
	if _, err := decodeBoundedJSON([]byte(`9007199254740991.1`), 1024); err == nil {
		t.Fatal("rounded fraction")
	}
	if _, err := decodeBoundedJSON([]byte(`1e-99999`), 1024); err == nil {
		t.Fatal("underflow")
	}
	if _, err := decodeBoundedJSON([]byte(`9007199254740992`), 1024); err == nil {
		t.Fatal("unsafe integer")
	}
	if _, err := decodeBoundedJSON([]byte{34, 255, 34}, 1024); err == nil {
		t.Fatal("invalid UTF8")
	}
}

func TestTaskStructuredResultTrustBoundary(t *testing.T) {
	req := TaskRunRequest{OutputSchema: resultSchema}
	raw := json.RawMessage(`{"ok":true,"findings":[]}`)
	finish := TaskRunFinish{State: TaskCompleted, Result: TaskRunResult{Output: string(raw), StructuredOutput: raw}}
	if err := ValidateTaskStructuredResult(req, finish); err != nil {
		t.Fatal(err)
	}
	bad := finish
	bad.Result.Output = `{"ok":false,"findings":[]}`
	if err := ValidateTaskStructuredResult(req, bad); err == nil {
		t.Fatal("accepted disagreeing text")
	}
	bad = finish
	bad.Result.StructuredOutput = nil
	if err := ValidateTaskStructuredResult(req, bad); err == nil {
		t.Fatal("missing structured result")
	}
	if err := ValidateTaskStructuredResult(TaskRunRequest{}, finish); err == nil {
		t.Fatal("unsolicited structured result")
	}
	bad = finish
	bad.State = TaskFailed
	if err := ValidateTaskStructuredResult(req, bad); err == nil {
		t.Fatal("failed result claimed validation")
	}
	if err := ValidateTaskStructuredResult(req, TaskRunFinish{State: TaskFailed}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTaskRun(&TaskRunRequest{Prompt: "x", OutputSchema: []byte(`{"$ref":"https://example.com"}`)}); err == nil {
		t.Fatal("request bypassed schema validation")
	}
}
