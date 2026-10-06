// Package policy evaluates a seat's committed capability manifest. It is the
// single place the platform decides what a seat may do; transports never
// change the answer (§7.2, §10.1).
package policy

import (
	"slices"
	"strings"

	"github.com/darcys22/steadmesh/pkg/compile"
)

// Capability returns the seat's capability on resource (memory:<k>, connection:<k>).
func Capability(sm *compile.SeatManifest, resource string) (compile.Capability, bool) {
	for _, c := range sm.Capabilities {
		if c.Resource == resource {
			return c, true
		}
	}
	return compile.Capability{}, false
}

// Allows reports whether the seat holds op on resource.
func Allows(sm *compile.SeatManifest, resource, op string) bool {
	c, ok := Capability(sm, resource)
	return ok && slices.Contains(c.Operations, op)
}

// MemoryStores returns the keys of the memory stores on which the seat holds op.
func MemoryStores(sm *compile.SeatManifest, op string) []string {
	var out []string
	for _, c := range sm.Capabilities {
		if key, ok := strings.CutPrefix(c.Resource, "memory:"); ok && slices.Contains(c.Operations, op) {
			out = append(out, key)
		}
	}
	return out
}

// HasMemory reports whether the seat holds op on any memory store.
func HasMemory(sm *compile.SeatManifest, op string) bool { return len(MemoryStores(sm, op)) > 0 }

// Connections returns the seat's connection capabilities keyed by connection.
func Connections(sm *compile.SeatManifest) map[string]compile.Capability {
	out := map[string]compile.Capability{}
	for _, c := range sm.Capabilities {
		if key, ok := strings.CutPrefix(c.Resource, "connection:"); ok {
			out[key] = c
		}
	}
	return out
}

// ConnectionAllowed checks a gateway call against the grant for the
// connection: the operation must be granted, and when the grant restricts
// targets the call's target must match one of them.
func ConnectionAllowed(sm *compile.SeatManifest, connection, op, target string) bool {
	c, ok := Capability(sm, "connection:"+connection)
	if !ok || !slices.Contains(c.Operations, op) {
		return false
	}
	return len(c.Targets) == 0 || MatchTarget(c.Targets, target)
}

// MatchTarget matches target against grant targets. A target pattern is an
// exact value, "*", or a prefix ending in "*".
func MatchTarget(patterns []string, target string) bool {
	if target == "" {
		return false
	}
	for _, p := range patterns {
		if p == "*" || p == target {
			return true
		}
		if prefix, ok := strings.CutSuffix(p, "*"); ok && strings.HasPrefix(target, prefix) {
			return true
		}
	}
	return false
}

// SendEdge returns the declared route from the seat to recipient.
func SendEdge(sm *compile.SeatManifest, recipient string) (compile.RouteEdge, bool) {
	for _, e := range sm.SendTo {
		if e.Seat == recipient {
			return e, true
		}
	}
	return compile.RouteEdge{}, false
}

// CanReplyToSeat reports whether the seat may reply to a message from sender:
// it has its own route to the sender, or the sender's route permits replies.
func CanReplyToSeat(sm *compile.SeatManifest, sender string) bool {
	if _, ok := SendEdge(sm, sender); ok {
		return true
	}
	for _, e := range sm.ReceiveFrom {
		if e.Seat == sender && e.Reply {
			return true
		}
	}
	return false
}

// HasBinding reports whether the seat is the representative of binding.
func HasBinding(sm *compile.SeatManifest, binding string) bool {
	return slices.Contains(sm.ChannelBindings, binding)
}

// Reachable returns the seats the seat can message or that can message it.
func Reachable(sm *compile.SeatManifest) []string {
	var out []string
	for _, edges := range [][]compile.RouteEdge{sm.SendTo, sm.ReceiveFrom} {
		for _, e := range edges {
			if !slices.Contains(out, e.Seat) {
				out = append(out, e.Seat)
			}
		}
	}
	slices.Sort(out)
	return out
}
