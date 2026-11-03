package harnesses

import "testing"

func TestRegistry(t *testing.T) {
	Register(Descriptor{Name: "test-reg", Aliases: []string{"test-reg-alias"}, APIs: []string{APIOpenAIChat}, New: func() Adapter { return nil }})
	if d, ok := Lookup("test-reg-alias"); !ok || d.Name != "test-reg" {
		t.Fatalf("alias lookup: %+v %v", d, ok)
	}
	if _, ok := Lookup("nope"); ok {
		t.Fatal("unknown name found")
	}
	for _, d := range []Descriptor{
		{Name: "test-reg", New: func() Adapter { return nil }},
		{Name: "x", Aliases: []string{"test-reg-alias"}, New: func() Adapter { return nil }},
		{Name: "y", APIs: []string{"grpc"}, New: func() Adapter { return nil }},
		{Name: "z"},
		{Name: "bad-effort-key", Effort: &EffortMapping{Setting: "missing", Values: map[string]string{"high": "high"}}, New: func() Adapter { return nil }},
		{Name: "bad-effort-value", Settings: map[string]Setting{"effort": {Values: []string{"low"}}}, Effort: &EffortMapping{Setting: "effort", Values: map[string]string{"high": "high"}}, New: func() Adapter { return nil }},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%+v registered", d)
				}
			}()
			Register(d)
		}()
	}
}
