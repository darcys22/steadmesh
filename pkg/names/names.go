// Package names derives deterministic, length-safe Kubernetes names for seat
// objects (docs/architecture.html).
package names

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Seat returns the base name for the objects of seat seatKey in organisation orgKey.
func Seat(orgKey, seatKey string) string {
	base := "seat-" + strings.ReplaceAll(strings.ToLower(seatKey), "_", "-")
	if len(base) > 40 {
		base = strings.TrimRight(base[:40], "-")
	}
	sum := sha256.Sum256([]byte(orgKey + "/" + seatKey))
	return base + "-" + hex.EncodeToString(sum[:])[:8]
}

// Pod is the name of the seat's single Pod (StatefulSet ordinal 0).
func Pod(seatName string) string { return seatName + "-0" }

// WorkspaceClaim is the name of the seat's retained workspace PVC.
func WorkspaceClaim(seatName string) string { return "ws-" + seatName }

// ManifestConfigMap is the name of the seat's manifest ConfigMap.
func ManifestConfigMap(seatName string) string { return seatName + "-manifest" }
