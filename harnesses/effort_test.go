package harnesses

import (
	"reflect"
	"strings"
	"testing"
)

func TestResolveEffort(t *testing.T) {
	mapping := &EffortMapping{Setting: "thinking", Values: map[string]string{"none": "off", "high": "high"}}
	for _, tc := range []struct {
		name, effort string
		settings     map[string]string
		mapping      *EffortMapping
		want         map[string]string
		wantErr      string
	}{
		{"default", "", nil, nil, nil, ""},
		{"native preserved", "", map[string]string{"thinking": "off"}, mapping, map[string]string{"thinking": "off"}, ""},
		{"translated", "none", nil, mapping, map[string]string{"thinking": "off"}, ""},
		{"same value", "high", map[string]string{"thinking": "high", "max_tokens": "10"}, mapping, map[string]string{"thinking": "high", "max_tokens": "10"}, ""},
		{"conflict", "high", map[string]string{"thinking": "off"}, mapping, nil, "conflicts"},
		{"unsupported", "max", nil, mapping, nil, "unsupported effort"},
		{"unavailable", "high", nil, nil, nil, "does not support"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveEffort(tc.effort, tc.settings, tc.mapping)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error=%v", err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got=%v error=%v want=%v", got, err, tc.want)
			}
			if got != nil {
				got["mutation"] = "test"
				if _, exists := tc.settings["mutation"]; exists {
					t.Fatal("input map mutated")
				}
			}
		})
	}
}
