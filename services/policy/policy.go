// Package policy evaluates a seat's committed capability manifest. It is the
// single place the platform decides what a seat may do; transports never
// change the answer (§7.2, §10.1).
package policy

import (
	"slices"
	"strings"

	"github.com/darcys22/steadmesh/pkg/compile"
)

// Capability returns a resource's capability only when exactly one clause
// exists. It returns false for an absent or ambiguous resource.
//
// Deprecated: use Allows or ConnectionAllowed for authorization; a resource
// can have several independent capability clauses.
func Capability(sm *compile.SeatManifest, resource string) (compile.Capability, bool) {
	var found compile.Capability
	ok := false
	for _, c := range sm.Capabilities {
		if c.Resource == resource {
			if ok {
				return compile.Capability{}, false
			}
			found, ok = c, true
		}
	}
	return found, ok
}

// Allows reports whether any clause grants op on resource. It does not
// authorize a target; connection callers must use ConnectionAllowed.
func Allows(sm *compile.SeatManifest, resource, op string) bool {
	for _, c := range sm.Capabilities {
		if c.Resource == resource && slices.Contains(c.Operations, op) {
			return true
		}
	}
	return false
}

// MemoryStores returns the keys of the memory stores on which the seat holds op.
func MemoryStores(sm *compile.SeatManifest, op string) []string {
	var out []string
	for _, c := range sm.Capabilities {
		if key, ok := strings.CutPrefix(c.Resource, "memory:"); ok && slices.Contains(c.Operations, op) {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// HasMemory reports whether the seat holds op on any memory store.
func HasMemory(sm *compile.SeatManifest, op string) bool { return len(MemoryStores(sm, op)) > 0 }

// Connections returns all connection clauses, preserving their operation/target
// pairings. A connection can have more than one independently scoped clause.
func Connections(sm *compile.SeatManifest) map[string][]compile.Capability {
	out := map[string][]compile.Capability{}
	for _, c := range sm.Capabilities {
		if key, ok := strings.CutPrefix(c.Resource, "connection:"); ok {
			out[key] = append(out[key], c)
		}
	}
	return out
}

// ConnectionAllowed checks a gateway call against independent grant clauses.
// A single clause must grant both the operation and its target; fields from
// different clauses must never be combined to authorize a call.
func ConnectionAllowed(sm *compile.SeatManifest, connection, op, target string) bool {
	for _, c := range sm.Capabilities {
		if c.Resource == "connection:"+connection && slices.Contains(c.Operations, op) &&
			(len(c.Targets) == 0 || MatchTarget(c.Targets, target)) {
			return true
		}
	}
	return false
}

// ConnectionTargetsAllowed authorizes all resources resolved by a connector.
// An empty set denotes an account-wide or unresolved operation and requires an
// unrestricted clause for that operation. No other operation's unrestricted
// grant can be borrowed. Empty entries are invalid concrete targets.
func ConnectionTargetsAllowed(sm *compile.SeatManifest, connection, op string, targets []string) bool {
	if len(targets) == 0 {
		return ConnectionAllowed(sm, connection, op, "")
	}
	for _, target := range targets {
		if target == "" || !ConnectionAllowed(sm, connection, op, target) {
			return false
		}
	}
	return true
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
