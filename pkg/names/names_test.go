package names

import (
	"strings"
	"testing"
)

func TestSeat(t *testing.T) {
	a := Seat("org", "representative_sean")
	if a != Seat("org", "representative_sean") || !strings.HasPrefix(a, "seat-representative-sean-") {
		t.Fatalf("unexpected %q", a)
	}
	if Seat("org", "x") == Seat("other", "x") {
		t.Fatal("names must differ per organisation")
	}
	long := Seat("org", strings.Repeat("a", 63))
	if len(WorkspaceClaim(long)) > 63 || len(ManifestConfigMap(long)) > 63 {
		t.Fatalf("too long: %d", len(ManifestConfigMap(long)))
	}
}
