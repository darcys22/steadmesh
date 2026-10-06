// Package controller contains the AgentOrganization and AgentSeat
// reconcilers (design §6, contracts.md "Readiness flow" and "Lifecycle
// decisions").
package controller

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/darcys22/steadmesh/api/v1alpha1"
	"github.com/darcys22/steadmesh/pkg/compile"
	seatruntime "github.com/darcys22/steadmesh/runtime"
	"github.com/darcys22/steadmesh/runtime/kube"
)

// FieldOwner is the controller's field manager. It writes only status (and
// controller metadata: finalizers, verify-observed) on AgentOrganization.
const FieldOwner = "steadmesh-controller"

// AnnotationVerifyRequest on AgentSeats propagates the organisation's verify nonce.
const AnnotationSeatVerifyRequest = v1alpha1.AnnotationVerifyRequest

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func seatOwnerRef(seat *v1alpha1.AgentSeat) *metav1.OwnerReference {
	return &metav1.OwnerReference{
		APIVersion:         v1alpha1.GroupVersion.String(),
		Kind:               "AgentSeat",
		Name:               seat.Name,
		UID:                seat.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}
}

// backendSeat builds the backend's view of a seat.
func backendSeat(seat *v1alpha1.AgentSeat, m compile.SeatManifest) *seatruntime.Seat {
	return &seatruntime.Seat{
		Namespace:      seat.Namespace,
		Name:           seat.Name,
		OrgKey:         seat.Labels[v1alpha1.LabelOrganization],
		OrgID:          seat.Spec.OrganizationID,
		SeatKey:        seat.Spec.SeatKey,
		SeatID:         seat.Spec.SeatID,
		ConfigRevision: seat.Spec.ConfigRevision,
		Manifest:       m,
		Owner:          seatOwnerRef(seat),
		AdoptFrom:      m.AdoptFrom,
	}
}

// manifestFiles renders the manifest ConfigMap content.
func manifestFiles(sm compile.SeatManifest, texts map[string]string) (map[string]string, error) {
	b, err := json.MarshalIndent(sm, "", "  ")
	if err != nil {
		return nil, err
	}
	return map[string]string{
		kube.ManifestFile:     string(b),
		kube.InstructionsFile: RenderInstructions(sm.Instructions, texts),
	}, nil
}

func parseManifest(data string) (compile.SeatManifest, error) {
	var sm compile.SeatManifest
	if err := json.Unmarshal([]byte(data), &sm); err != nil {
		return sm, fmt.Errorf("parse %s: %w", kube.ManifestFile, err)
	}
	return sm, nil
}

func idleTimeout(e string) time.Duration {
	d, err := time.ParseDuration(e)
	if err != nil {
		return 15 * time.Minute
	}
	return d
}

func timePtr(t *time.Time) *metav1.Time {
	if t == nil {
		return nil
	}
	mt := metav1.NewTime(t.UTC().Truncate(time.Second))
	return &mt
}
