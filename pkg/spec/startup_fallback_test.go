package spec

import "testing"

func TestHarnessProfileStartupFallbackDeepCopy(t *testing.T) {
	original := &HarnessProfile{StartupFallback: &StartupFallbackPolicy{Profiles: []string{"secondary"}, MaxAttempts: 2, TotalTimeout: "30s"}}
	copy := original.DeepCopy()
	copy.StartupFallback.Profiles[0] = "changed"
	copy.StartupFallback.MaxAttempts = 1
	if original.StartupFallback.Profiles[0] != "secondary" || original.StartupFallback.MaxAttempts != 2 {
		t.Fatal("copy mutated the original policy")
	}
}
