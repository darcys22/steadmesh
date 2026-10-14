package connectors

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestDecodeParamsRejectsAmbiguousObjects(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `"repo"`, `{`, `{} {}`, `{"repo":"a/b","repo":"c/d"}`, `{"repo":"a/b","Repo":"c/d"}`, `{"repo":"a/b","target":"c/d"}`, `{"repo":5}`} {
		t.Run(raw, func(t *testing.T) {
			var p struct {
				Repo string `json:"repo"`
			}
			if err := DecodeParams(json.RawMessage(raw), &p); !errors.Is(err, ErrPermanent) {
				t.Fatalf("accepted ambiguous params: %v", err)
			}
		})
	}
}

func TestParamsObjectPreservesIntegers(t *testing.T) {
	fields, err := ParamsObject(json.RawMessage(`{"number":9007199254740993,"repo":"acme/repo"}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Number int64  `json:"number"`
		Repo   string `json:"repo"`
	}
	if err := DecodeParams(raw, &p); err != nil || p.Number != 9007199254740993 {
		t.Fatalf("number=%d: %v", p.Number, err)
	}
}
