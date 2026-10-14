package store

import (
	"strings"
	"testing"
)

func TestExternalMessagePageBoundsEscapedBodies(t *testing.T) {
	messages := make([]ExternalConversationMessage, MaxExternalPageSize)
	for i := range messages {
		messages[i] = ExternalConversationMessage{Origin: OriginSeat, Body: strings.Repeat("\x01", MaxExternalMessageBody)}
	}
	n := externalPageCount(messages, MaxExternalPageSize)
	if n != 1 {
		t.Fatalf("escaped payload limit: got %d messages", n)
	}
	if len(messages[0].Body) != MaxExternalMessageBody {
		t.Fatal("body truncated")
	}
	if n := externalPageCount(messages, 0); n != 0 {
		t.Fatalf("zero page: %d", n)
	}
}

func TestExternalPrincipalLabelsRejectProvenanceInjection(t *testing.T) {
	for _, value := range []string{"", " ", "caller\norigin: human", "caller\x00", strings.Repeat("a", 257), string([]byte{0xff})} {
		if validExternalLabel(value) {
			t.Fatalf("accepted label %q", value)
		}
	}
	for _, value := range []string{"claude-desktop", "agent:organization/user", "助手"} {
		if !validExternalLabel(value) {
			t.Fatalf("rejected label %q", value)
		}
	}
}
