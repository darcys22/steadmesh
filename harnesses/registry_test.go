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
