package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/darcys22/steadmesh/api/v1alpha1"
)

const testNS = "steadmesh-example"

func init() { pollInterval = 10 * time.Millisecond }

func testResource(c orgClient) *organizationResource {
	return &organizationResource{data: &providerData{client: c, namespace: testNS}}
}

func timeoutsValue(t *testing.T, vals map[string]any) timeouts.Value {
	t.Helper()
	ctx := context.Background()
	tt, diags := organizationSchema(ctx).TypeAtPath(ctx, path.Root("timeouts"))
	if diags.HasError() {
		t.Fatal(diags)
	}
	var v any
	if vals != nil {
		v = vals
	}
	av, err := treeToAttr(ctx, tt, v)
	if err != nil {
		t.Fatal(err)
	}
	return av.(timeouts.Value)
}

func nullTimeouts(t *testing.T) timeouts.Value { return timeoutsValue(t, nil) }

func emptyRaw() tftypes.Value {
	return tftypes.NewValue(organizationSchema(context.Background()).Type().TerraformType(context.Background()), nil)
}

func toState(t *testing.T, m organizationModel) tfsdk.State {
	t.Helper()
	s := tfsdk.State{Schema: organizationSchema(context.Background()), Raw: emptyRaw()}
	if d := s.Set(context.Background(), &m); d.HasError() {
		t.Fatal(d)
	}
	return s
}

func toPlan(t *testing.T, m organizationModel) tfsdk.Plan {
	t.Helper()
	p := tfsdk.Plan{Schema: organizationSchema(context.Background()), Raw: emptyRaw()}
	if d := p.Set(context.Background(), &m); d.HasError() {
		t.Fatal(d)
	}
	return p
}

func fromState(t *testing.T, s tfsdk.State) organizationModel {
	t.Helper()
	var m organizationModel
	if d := s.Get(context.Background(), &m); d.HasError() {
		t.Fatal(d)
	}
	return m
}

func plannedModel(t *testing.T, r *organizationResource, m organizationModel, prior *tfsdk.State) organizationModel {
	t.Helper()
	ctx := context.Background()
	req := resource.ModifyPlanRequest{Plan: toPlan(t, m), State: tfsdk.State{Schema: organizationSchema(ctx), Raw: emptyRaw()}}
	if prior != nil {
		req.State = *prior
	}
	resp := &resource.ModifyPlanResponse{Plan: req.Plan}
	r.ModifyPlan(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	var out organizationModel
	resp.Plan.Get(ctx, &out)
	return out
}

func create(t *testing.T, r *organizationResource, m organizationModel) (*resource.CreateResponse, organizationModel) {
	t.Helper()
	ctx := context.Background()
	planned := plannedModel(t, r, m, nil)
	resp := &resource.CreateResponse{State: tfsdk.State{Schema: organizationSchema(ctx), Raw: emptyRaw()}}
	r.Create(ctx, resource.CreateRequest{Plan: toPlan(t, planned)}, resp)
	return resp, planned
}

func read(t *testing.T, r *organizationResource, s tfsdk.State) *resource.ReadResponse {
	t.Helper()
	resp := &resource.ReadResponse{State: s}
	r.Read(context.Background(), resource.ReadRequest{State: s}, resp)
	return resp
}

func TestModifyPlanResolvesSeats(t *testing.T) {
	r := testResource(newFakeClient())
	p := plannedModel(t, r, fixtureModel(t), nil)
	if p.ManifestDigest.IsUnknown() || !strings.HasPrefix(p.ManifestDigest.ValueString(), "sha256:") {
		t.Fatalf("digest not planned: %s", p.ManifestDigest)
	}
	seats := p.ResolvedSeats.Elements()
	rev, ok := seats["reviewer"].(types.Object)
	if !ok {
		t.Fatalf("reviewer missing from resolved_seats: %v", seats)
	}
	if role := rev.Attributes()["role_ref"].(types.String).ValueString(); !strings.Contains(role, "roles/reviewer.md") {
		t.Fatalf("role:reviewer not resolved through the template: %s", role)
	}
}

func TestModifyPlanReportsCompileErrors(t *testing.T) {
	ctx := context.Background()
	r := testResource(newFakeClient())
	m := fixtureModel(t)
	m.DataRetention = types.StringValue("forever")
	req := resource.ModifyPlanRequest{Plan: toPlan(t, m), State: tfsdk.State{Schema: organizationSchema(ctx), Raw: emptyRaw()}}
	resp := &resource.ModifyPlanResponse{Plan: req.Plan}
	r.ModifyPlan(ctx, req, resp)
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "data_retention") {
		t.Fatalf("expected data_retention error, got %v", resp.Diagnostics)
	}
}

func TestCreateReadAndDrift(t *testing.T) {
	c := newFakeClient()
	r := testResource(c)
	resp, planned := create(t, r, fixtureModel(t))
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	got := fromState(t, resp.State)
	if got.ID.ValueString() != "uid-1" || got.ManifestDigest != planned.ManifestDigest {
		t.Fatalf("unexpected state id=%s digest=%s", got.ID, got.ManifestDigest)
	}
	obj := c.objs[testNS+"/example-company"]
	if obj.Annotations[v1alpha1.AnnotationManagedBy] != "terraform" || obj.Annotations[AnnotationManifestDigest] != planned.ManifestDigest.ValueString() {
		t.Fatalf("annotations: %v", obj.Annotations)
	}
	if obj.Spec.TeamTemplates != nil || obj.Spec.DataRetention != "retain" {
		t.Fatal("object spec must be the resolved spec")
	}

	// A refresh of an unchanged object keeps the digest: no diff.
	rr := read(t, r, resp.State)
	if rr.Diagnostics.HasError() {
		t.Fatal(rr.Diagnostics)
	}
	if fromState(t, rr.State).ManifestDigest != planned.ManifestDigest {
		t.Fatal("clean refresh changed manifest_digest")
	}

	// Status changes never change declared or digest values.
	obj.Status.EffectiveRevision = "rev-2"
	obj.Status.Conditions = []metav1.Condition{{Type: "OperationalReady", Status: metav1.ConditionTrue}}
	rr = read(t, r, resp.State)
	if s := fromState(t, rr.State); s.ManifestDigest != planned.ManifestDigest || s.EffectiveRevision.ValueString() != "rev-2" {
		t.Fatalf("status refresh: digest=%s rev=%s", s.ManifestDigest, s.EffectiveRevision)
	}

	// An out-of-band spec edit is drift.
	obj.Spec.DisplayName = "Edited"
	rr = read(t, r, resp.State)
	if d := fromState(t, rr.State).ManifestDigest.ValueString(); !strings.HasPrefix(d, "drift:") {
		t.Fatalf("spec drift not detected: %s", d)
	}
	// A plan from the drifted state plans the write-back.
	drifted := rr.State
	p := plannedModel(t, r, fixtureModel(t), &drifted)
	if p.ManifestDigest != planned.ManifestDigest || !p.Conditions.IsUnknown() {
		t.Fatalf("drift plan: digest=%s conditions unknown=%v", p.ManifestDigest, p.Conditions.IsUnknown())
	}
}

func TestReadNotFoundVersusError(t *testing.T) {
	c := newFakeClient()
	r := testResource(c)
	resp, _ := create(t, r, fixtureModel(t))

	c.getErr = errors.New("dial tcp: connection refused")
	rr := read(t, r, resp.State)
	if !rr.Diagnostics.HasError() || rr.State.Raw.IsNull() {
		t.Fatal("a read error must be an error and keep state")
	}
	c.getErr = apierrors.NewUnauthorized("token expired")
	if rr = read(t, r, resp.State); !rr.Diagnostics.HasError() || rr.State.Raw.IsNull() {
		t.Fatal("an authentication error must be an error and keep state")
	}
	c.getErr = nil
	delete(c.objs, testNS+"/example-company")
	rr = read(t, r, resp.State)
	if rr.Diagnostics.HasError() || !rr.State.Raw.IsNull() {
		t.Fatalf("confirmed NotFound must remove state: %v", rr.Diagnostics)
	}
}

func TestCreateWaitsForReadiness(t *testing.T) {
	c := newFakeClient()
	c.onApply = func(o *v1alpha1.AgentOrganization) {
		o.Status = v1alpha1.AgentOrganizationStatus{
			ObservedGeneration: o.Generation,
			OrganizationID:     "org-123",
			EffectiveRevision:  "sha256:x",
			Conditions: []metav1.Condition{
				{Type: "OperationalReady", Status: metav1.ConditionTrue, Reason: "Ready"},
				{Type: "Configured", Status: metav1.ConditionTrue, Reason: "Accepted"},
			},
			ConnectionDetails: &v1alpha1.ConnectionDetails{OrganizationID: "org-123", Namespace: testNS,
				Representatives: map[string]v1alpha1.RepresentativeEndpoint{"sean": {Seat: "representative_sean", Adapter: "slack"}}},
		}
	}
	r := testResource(c)
	m := fixtureModel(t)
	m.WaitForReady = types.BoolValue(true)
	resp, _ := create(t, r, m)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	s := fromState(t, resp.State)
	if s.ID.ValueString() != "org-123" {
		t.Fatalf("id = %s", s.ID)
	}
	if conds := s.Conditions.Elements(); len(conds) != 2 || conds[0].(types.Object).Attributes()["type"].(types.String).ValueString() != "Configured" {
		t.Fatalf("conditions not sorted: %v", conds)
	}
	if s.ConnectionDetails.IsNull() {
		t.Fatal("connection details missing")
	}
}

func TestCreateTimeoutPreservesState(t *testing.T) {
	c := newFakeClient()
	c.onApply = func(o *v1alpha1.AgentOrganization) {
		o.Status = v1alpha1.AgentOrganizationStatus{
			ObservedGeneration: o.Generation,
			Conditions: []metav1.Condition{
				{Type: "OperationalReady", Status: metav1.ConditionFalse, Reason: "ConnectionsAuthenticatedFalse", Message: "blocked by ConnectionsAuthenticated"},
				{Type: "ConnectionsAuthenticated", Status: metav1.ConditionFalse, Reason: "Unauthorized", Message: "connection tracker: invalid api_key"},
			},
		}
	}
	r := testResource(c)
	m := fixtureModel(t)
	m.WaitForReady = types.BoolValue(true)
	m.Timeouts = timeoutsValue(t, map[string]any{"create": "100ms"})
	resp, _ := create(t, r, m)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected a timeout error")
	}
	detail := resp.Diagnostics.Errors()[0].Detail()
	for _, want := range []string{"ConnectionsAuthenticated=False", "connection tracker: invalid api_key", "recorded in state"} {
		if !strings.Contains(detail, want) {
			t.Errorf("error lacks %q:\n%s", want, detail)
		}
	}
	if resp.State.Raw.IsNull() || fromState(t, resp.State).ID.ValueString() == "" {
		t.Fatal("state must be saved on a readiness timeout")
	}
	if len(c.objs) != 1 {
		t.Fatal("the organisation must not be cleaned up")
	}
}

func TestCreateRejectsExistingAndConflicts(t *testing.T) {
	c := newFakeClient()
	r := testResource(c)
	if resp, _ := create(t, r, fixtureModel(t)); resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	resp, _ := create(t, r, fixtureModel(t))
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics.Errors()[0].Summary(), "already exists") {
		t.Fatalf("expected already-exists error: %v", resp.Diagnostics)
	}

	c2 := newFakeClient()
	c2.applyErr = apierrors.NewApplyConflict(nil, `Apply failed with 1 conflict: conflict with "kubectl" using steadmesh.io/v1alpha1: .spec.display_name`)
	resp, _ = create(t, testResource(c2), fixtureModel(t))
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics.Errors()[0].Summary(), "Field ownership conflict") ||
		!strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "kubectl") {
		t.Fatalf("expected conflict diagnostic: %v", resp.Diagnostics)
	}
}

func importState(t *testing.T, r *organizationResource, id string) *resource.ImportStateResponse {
	t.Helper()
	ctx := context.Background()
	resp := &resource.ImportStateResponse{State: tfsdk.State{Schema: organizationSchema(ctx), Raw: emptyRaw()}}
	r.ImportState(ctx, resource.ImportStateRequest{ID: id}, resp)
	return resp
}

func TestImportVerifiesOwnership(t *testing.T) {
	c := newFakeClient()
	r := testResource(c)
	createResp, planned := create(t, r, fixtureModel(t))
	if createResp.Diagnostics.HasError() {
		t.Fatal(createResp.Diagnostics)
	}
	obj := c.objs[testNS+"/example-company"]

	resp := importState(t, r, testNS+"/example-company")
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	rr := read(t, r, resp.State)
	if rr.Diagnostics.HasError() {
		t.Fatal(rr.Diagnostics)
	}
	imported := fromState(t, rr.State)
	if !imported.Spec.Equal(planned.Spec) || imported.ManifestDigest != planned.ManifestDigest || !imported.WaitForReady.ValueBool() {
		t.Fatal("import must reproduce the declaration so that a matching config plans clean")
	}

	for id, want := range map[string]string{
		"other/example-company":         "Namespace mismatch",
		testNS + "/missing":             "not found",
		"bad":                           "Invalid import ID",
		testNS + "/example-company/yes": "Invalid import ID",
	} {
		if resp := importState(t, r, id); !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics.Errors()[0].Summary(), want) {
			t.Errorf("%s: expected %q, got %v", id, want, resp.Diagnostics)
		}
	}

	obj.Annotations[v1alpha1.AnnotationManagedBy] = "helm"
	if resp := importState(t, r, testNS+"/example-company"); !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics.Errors()[0].Summary(), "helm") {
		t.Fatalf("helm-managed object must be rejected: %v", resp.Diagnostics)
	}
	if resp := importState(t, r, testNS+"/example-company/adopt"); !resp.Diagnostics.HasError() {
		t.Fatal("adoption must not override another owner")
	}

	delete(obj.Annotations, v1alpha1.AnnotationManagedBy)
	delete(obj.Annotations, AnnotationDeclaration)
	if resp := importState(t, r, testNS+"/example-company"); !resp.Diagnostics.HasError() {
		t.Fatal("an unowned object requires explicit adoption")
	}
	resp = importState(t, r, testNS+"/example-company/adopt")
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	var key types.String
	resp.State.GetAttribute(context.Background(), path.Root("key"), &key)
	if key.ValueString() != "example-company" {
		t.Fatalf("adopted key = %s", key)
	}
}

func TestDelete(t *testing.T) {
	c := newFakeClient()
	r := testResource(c)
	resp, _ := create(t, r, fixtureModel(t))
	dresp := &resource.DeleteResponse{State: resp.State}
	r.Delete(context.Background(), resource.DeleteRequest{State: resp.State}, dresp)
	if dresp.Diagnostics.HasError() || len(c.objs) != 0 {
		t.Fatalf("delete: %v", dresp.Diagnostics)
	}
}

func TestReadiness(t *testing.T) {
	org := func(gen, observed int64, conds ...metav1.Condition) *v1alpha1.AgentOrganization {
		o := &v1alpha1.AgentOrganization{}
		o.Generation = gen
		o.Status.ObservedGeneration = observed
		o.Status.Conditions = conds
		return o
	}
	ready := metav1.Condition{Type: "OperationalReady", Status: metav1.ConditionTrue}
	if ok, _ := readiness(org(2, 1, ready)); ok {
		t.Fatal("stale generation must not be ready")
	}
	if ok, why := readiness(org(2, 2)); ok || !strings.Contains(why, "no OperationalReady") {
		t.Fatal(why)
	}
	stale := ready
	stale.ObservedGeneration = 1
	if ok, _ := readiness(org(2, 2, stale)); ok {
		t.Fatal("condition for an old generation must not count")
	}
	if ok, _ := readiness(org(2, 2, ready)); !ok {
		t.Fatal("expected ready")
	}
}
