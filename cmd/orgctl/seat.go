package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"text/tabwriter"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/darcys22/steadmesh/api/v1alpha1"
	"github.com/darcys22/steadmesh/pkg/names"
)

type seatOptions struct {
	kctx, namespace, organization, key, output string
	interval                                   time.Duration
}

func isSeatCommand(cmd string) bool {
	switch cmd {
	case "inspect", "watch", "pause", "resume":
		return true
	}
	return false
}

func (o seatOptions) validate() error {
	if strings.TrimSpace(o.namespace) == "" || strings.TrimSpace(o.organization) == "" || strings.TrimSpace(o.key) == "" {
		return errors.New("seat commands require --namespace, --name and --seat (the stable seat key)")
	}
	if o.output != "text" && o.output != "json" {
		return errors.New("--output must be text or json")
	}
	if o.interval <= 0 {
		return errors.New("--interval must be greater than zero")
	}
	return nil
}

// resolveSeat uses the same stable key-to-name mapping as the controller and
// checks the identity before acting. It never selects the first matching label,
// guesses a Pod name or falls back to a seat in another organisation.
func resolveSeat(ctx context.Context, c client.Client, o seatOptions) (*v1alpha1.AgentSeat, error) {
	var org v1alpha1.AgentOrganization
	if err := c.Get(ctx, types.NamespacedName{Namespace: o.namespace, Name: o.organization}, &org); err != nil {
		return nil, fmt.Errorf("get organisation: %w", err)
	}
	if org.Spec.Key == "" || org.Status.OrganizationID == "" {
		return nil, errors.New("organisation identity is not ready; wait for the controller to sync it")
	}
	var seat v1alpha1.AgentSeat
	key := types.NamespacedName{Namespace: o.namespace, Name: names.Seat(org.Spec.Key, o.key)}
	if err := c.Get(ctx, key, &seat); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("seat key %q has no AgentSeat yet; check the key with orgctl status or wait for reconciliation: %w", o.key, err)
		}
		return nil, fmt.Errorf("get AgentSeat: %w", err)
	}
	if seat.Spec.SeatKey != o.key || seat.Spec.OrganizationName != org.Name || seat.Spec.OrganizationID != org.Status.OrganizationID {
		return nil, fmt.Errorf("AgentSeat %s has a different seat or organisation identity; refusing to use it", key)
	}
	if seat.Spec.SeatID == "" || seat.UID == "" || seat.ResourceVersion == "" {
		return nil, errors.New("seat identity is not ready; wait for the controller to sync it")
	}
	return &seat, nil
}

func seatCommand(ctx context.Context, c client.Client, cmd string, o seatOptions, out io.Writer) error {
	if err := o.validate(); err != nil {
		return err
	}
	seat, err := resolveSeat(ctx, c, o)
	if err == nil {
		switch cmd {
		case "inspect":
			err = printSeat(out, o, seat)
		case "watch":
			err = watchSeat(ctx, c, o, seat, out)
		case "pause", "resume":
			changed := false
			changed, err = setSeatSuspended(ctx, c, seat, cmd == "pause")
			if err == nil {
				if o.output == "text" {
					if changed {
						_, err = fmt.Fprintln(out, "Administrative suspension updated; the controller applies it asynchronously.")
					} else {
						_, err = fmt.Fprintln(out, "Administrative suspension already has the requested value; no change made.")
					}
				}
				if err == nil {
					err = printSeat(out, o, seat)
				}
			}
		default:
			err = fmt.Errorf("unknown seat command %q", cmd)
		}
	}
	if err != nil {
		return fmt.Errorf("context %q, organisation %s/%s, seat %q: %w", o.kctx, o.namespace, o.organization, o.key, err)
	}
	return nil
}

// setSeatSuspended patches only the existing administrative control. The
// resource-version precondition rejects concurrent changes (including object
// replacement); it must never overwrite a new configuration or another
// operator's more recent decision.
func setSeatSuspended(ctx context.Context, c client.Client, seat *v1alpha1.AgentSeat, suspended bool) (bool, error) {
	if seat.Spec.AdminSuspended == suspended {
		return false, nil
	}
	if seat.ResourceVersion == "" {
		return false, errors.New("seat has no resource version; refusing an unguarded suspension change")
	}
	data, err := json.Marshal(map[string]any{
		"metadata": map[string]string{"resourceVersion": seat.ResourceVersion},
		"spec":     map[string]bool{"adminSuspended": suspended},
	})
	if err != nil {
		return false, err
	}
	patch := client.RawPatch(types.MergePatchType, data)
	if err := c.Patch(ctx, seat, patch); err != nil {
		if apierrors.IsConflict(err) {
			return false, fmt.Errorf("seat changed while updating suspension; inspect it and retry: %w", err)
		}
		return false, fmt.Errorf("patch adminSuspended (requires patch permission on agentseats): %w", err)
	}
	return true, nil
}

// seatInspection deliberately excludes arbitrary annotations and managed
// fields. Everything here comes from the non-secret AgentSeat spec/status;
// inspecting a seat does not fetch credentials, logs or workspace contents.
type seatInspection struct {
	Context      string                   `json:"context"`
	Namespace    string                   `json:"namespace"`
	Organization string                   `json:"organization"`
	Name         string                   `json:"name"`
	UID          types.UID                `json:"uid"`
	Generation   int64                    `json:"generation"`
	Spec         v1alpha1.AgentSeatSpec   `json:"spec"`
	Status       v1alpha1.AgentSeatStatus `json:"status"`
}

func inspectSeat(o seatOptions, seat *v1alpha1.AgentSeat) seatInspection {
	return seatInspection{Context: o.kctx, Namespace: seat.Namespace, Organization: o.organization,
		Name: seat.Name, UID: seat.UID, Generation: seat.Generation, Spec: seat.Spec, Status: seat.Status}
}

func printSeat(out io.Writer, o seatOptions, seat *v1alpha1.AgentSeat) error {
	if o.output == "json" {
		// Each watch update is one JSON object, suitable for line-oriented tools.
		return json.NewEncoder(out).Encode(inspectSeat(o, seat))
	}
	w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	fmt.Fprintf(w, "\nContext\t%s\nOrganisation\t%s/%s\nSeat\t%s\nObject\t%s\nSeat ID\t%s\n",
		o.kctx, seat.Namespace, o.organization, seat.Spec.SeatKey, seat.Name, seat.Spec.SeatID)
	fmt.Fprintf(w, "Admin suspended\t%t\nState\t%s\nGeneration\t%d (observed %d)\nConfigured revision\t%s\nAdopted revision\t%s\n",
		seat.Spec.AdminSuspended, seat.Status.ExecutionState, seat.Generation, seat.Status.ObservedGeneration,
		seat.Spec.ConfigRevision, seat.Status.AdoptedRevision)
	fmt.Fprintf(w, "Harness\t%s\nExecution profile\t%s\nSandbox profile\t%s\nWorkspace claim\t%s\nBackend\t%s\nLease generation\t%d\nLatest checkpoint\t%s\n",
		seat.Spec.HarnessProfile, seat.Spec.ExecutionProfile, seat.Spec.SandboxProfile, seat.Spec.Workspace.ClaimName,
		seat.Status.BackendRef, seat.Status.LeaseGeneration, seat.Status.LatestCheckpoint)
	if seat.Spec.Retired {
		fmt.Fprintln(w, "Retirement requested\ttrue")
	}
	if seat.Status.RetiringUntil != nil {
		fmt.Fprintf(w, "Retiring until\t%s\n", seat.Status.RetiringUntil.UTC().Format(time.RFC3339))
	}
	if seat.Status.LastActivityTime != nil {
		fmt.Fprintf(w, "Last activity\t%s\n", seat.Status.LastActivityTime.UTC().Format(time.RFC3339))
	}
	if seat.Status.LastError != "" {
		fmt.Fprintf(w, "Last error\t%s\n", seat.Status.LastError)
	}
	if p := seat.Status.Probe; p != nil {
		fmt.Fprintf(w, "Probe\t%s (%s): %s\n", p.Status, p.Revision, p.Detail)
	}
	if len(seat.Status.Conditions) > 0 {
		fmt.Fprintln(w, "\nCONDITION\tSTATUS\tREASON\tMESSAGE")
		for _, c := range seat.Status.Conditions {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", c.Type, c.Status, c.Reason, c.Message)
		}
	}
	return w.Flush()
}

// watchSeat polls the selected object, printing the initial snapshot and then
// spec/status changes. Polling needs only get permission and survives routine
// API watch expiry. Cancellation interrupts both requests and the interval.
func watchSeat(ctx context.Context, c client.Client, o seatOptions, seat *v1alpha1.AgentSeat, out io.Writer) error {
	if err := printSeat(out, o, seat); err != nil {
		return err
	}
	previous := inspectSeat(o, seat)
	ticker := time.NewTicker(o.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		var current v1alpha1.AgentSeat
		if err := c.Get(ctx, client.ObjectKeyFromObject(seat), &current); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("watch AgentSeat: %w", err)
		}
		if current.UID != seat.UID || current.Spec.SeatID != seat.Spec.SeatID || current.Spec.SeatKey != seat.Spec.SeatKey || current.Spec.OrganizationID != seat.Spec.OrganizationID || current.Spec.OrganizationName != seat.Spec.OrganizationName {
			return errors.New("seat identity changed while watching; inspect it again before continuing")
		}
		next := inspectSeat(o, &current)
		if !reflect.DeepEqual(previous, next) {
			if err := printSeat(out, o, &current); err != nil {
				return err
			}
			previous = next
		}
	}
}
