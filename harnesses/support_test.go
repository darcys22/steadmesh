package harnesses

import (
	"strings"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

func TestEnvelopeCarriesCurrentTimeInSeatZone(t *testing.T) {
	created := time.Date(2026, 10, 8, 1, 0, 0, 0, time.UTC)
	out := RenderEnvelope(Delivery{Message: runtimeapi.Envelope{MessageID: "m1", Origin: "schedule", CreatedAt: created, Body: "hi"},
		Timezone: "Australia/Melbourne"})
	if !strings.Contains(out, "created_at: 2026-10-08T01:00:00Z\n") {
		t.Fatalf("created_at missing:\n%s", out)
	}
	line := ""
	for l := range strings.SplitSeq(out, "\n") {
		if v, ok := strings.CutPrefix(l, "current_time: "); ok {
			line = v
		}
	}
	if !strings.HasSuffix(line, "(Australia/Melbourne)") || !(strings.Contains(line, "+11:00") || strings.Contains(line, "+10:00")) {
		t.Fatalf("current_time = %q", line)
	}
	if utc := RenderEnvelope(Delivery{Message: runtimeapi.Envelope{MessageID: "m2"}}); !strings.Contains(utc, "+00:00 (UTC)") {
		t.Fatalf("default zone:\n%s", utc)
	}
}

func TestBootstrapStatesTimezone(t *testing.T) {
	rep := RenderBootstrap(Environment{Bootstrap: runtimeapi.Bootstrap{Self: runtimeapi.Self{IsRepresentative: true, Timezone: "America/New_York"}}}, "")
	if !strings.Contains(rep, "Time zone: America/New_York, your human's.") {
		t.Fatalf("representative bootstrap:\n%s", rep)
	}
	seat := RenderBootstrap(Environment{}, "")
	if !strings.Contains(seat, "Time zone: UTC.") {
		t.Fatalf("seat bootstrap:\n%s", seat)
	}
}
