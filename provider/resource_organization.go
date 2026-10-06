package provider

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/darcys22/steadmesh/api/v1alpha1"
	"github.com/darcys22/steadmesh/pkg/compile"
)

const defaultTimeout = 20 * time.Minute

// pollInterval is the readiness and deletion polling interval.
var pollInterval = 2 * time.Second

var (
	_ resource.Resource                   = (*organizationResource)(nil)
	_ resource.ResourceWithConfigure      = (*organizationResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*organizationResource)(nil)
	_ resource.ResourceWithImportState    = (*organizationResource)(nil)
	_ resource.ResourceWithUpgradeState   = (*organizationResource)(nil)
	_ resource.ResourceWithValidateConfig = (*organizationResource)(nil)
)

type organizationResource struct {
	data *providerData
}

func newOrganizationResource() resource.Resource { return &organizationResource{} }

type organizationModel struct {
	ID                types.String   `tfsdk:"id"`
	Key               types.String   `tfsdk:"key"`
	DisplayName       types.String   `tfsdk:"display_name"`
	Spec              types.Object   `tfsdk:"spec"`
	DataRetention     types.String   `tfsdk:"data_retention"`
	WaitForReady      types.Bool     `tfsdk:"wait_for_ready"`
	Timeouts          timeouts.Value `tfsdk:"timeouts"`
	ManifestDigest    types.String   `tfsdk:"manifest_digest"`
	ResolvedSeats     types.Map      `tfsdk:"resolved_seats"`
	Conditions        types.List     `tfsdk:"conditions"`
	EffectiveRevision types.String   `tfsdk:"effective_revision"`
	ConnectionDetails types.Object   `tfsdk:"connection_details"`
}

func (r *organizationResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_organization"
}

func (r *organizationResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = organizationSchema(ctx)
}

func (r *organizationResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	d, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("got %T", req.ProviderData))
		return
	}
	r.data = d
}

// UpgradeState is the state migration hook (§5.6). Schema version 0 is the
// first version; future versions add upgraders keyed by the prior version.
func (r *organizationResource) UpgradeState(context.Context) map[int64]resource.StateUpgrader {
	return map[int64]resource.StateUpgrader{}
}

// ValidateConfig runs the compiler when the configuration is fully known, so
// `terraform validate` reports specification errors early.
func (r *organizationResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var m organizationModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	d, err := declarationFromModel(ctx, &m)
	if errors.Is(err, errUnknown) {
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Invalid organisation specification", err.Error())
		return
	}
	_, diags := compileDeclaration(ctx, d)
	resp.Diagnostics.Append(diags...)
}

func (r *organizationResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // destroy
	}
	var plan organizationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var state *organizationModel
	if !req.State.Raw.IsNull() {
		state = &organizationModel{}
		resp.Diagnostics.Append(req.State.Get(ctx, state)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}
	changed := state == nil || !req.Plan.Raw.Equal(req.State.Raw)

	d, err := declarationFromModel(ctx, &plan)
	switch {
	case errors.Is(err, errUnknown):
		plan.ManifestDigest = types.StringUnknown()
		plan.ResolvedSeats = types.MapUnknown(types.ObjectType{AttrTypes: resolvedSeatAttrTypes})
		changed = true
	case err != nil:
		resp.Diagnostics.AddError("Invalid organisation specification", err.Error())
		return
	default:
		m, diags := compileDeclaration(ctx, d)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		plan.ManifestDigest = types.StringValue(m.Digest)
		plan.ResolvedSeats = resolvedSeatsValue(m)
		if state != nil && (!state.ManifestDigest.Equal(plan.ManifestDigest) || !state.ResolvedSeats.Equal(plan.ResolvedSeats)) {
			changed = true
		}
	}
	if changed {
		// Observed status is refreshed by the apply; never plan stale values.
		plan.Conditions = types.ListUnknown(types.ObjectType{AttrTypes: conditionAttrTypes})
		plan.EffectiveRevision = types.StringUnknown()
		plan.ConnectionDetails = types.ObjectUnknown(connectionDetailsAttrTypes)
	}
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

func (r *organizationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.configured(&resp.Diagnostics) {
		return
	}
	var plan organizationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	timeout, diags := plan.Timeouts.Create(ctx, defaultTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ns, key := r.data.namespace, plan.Key.ValueString()
	if _, err := r.data.client.Get(ctx, ns, key); err == nil {
		resp.Diagnostics.AddError("Organisation already exists",
			fmt.Sprintf("AgentOrganization %s/%s already exists. Import it with `terraform import <address> %s/%s` instead of creating it.", ns, key, ns, key))
		return
	} else if !apierrors.IsNotFound(err) {
		resp.Diagnostics.AddError("Cannot read AgentOrganization", fmt.Sprintf("%s/%s: %v", ns, key, err))
		return
	}
	r.applyAndWait(ctx, &plan, timeout, "create", &resp.Diagnostics, func(m *organizationModel) diag.Diagnostics { return resp.State.Set(ctx, m) })
}

func (r *organizationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !r.configured(&resp.Diagnostics) {
		return
	}
	var plan organizationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	timeout, diags := plan.Timeouts.Update(ctx, defaultTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r.applyAndWait(ctx, &plan, timeout, "update", &resp.Diagnostics, func(m *organizationModel) diag.Diagnostics { return resp.State.Set(ctx, m) })
}

// applyAndWait writes the resolved organisation and, if requested, waits for
// readiness. Once the object is written, state is always saved, including on a
// readiness timeout, so the next refresh can recover (§5.4). Nothing is ever
// cleaned up on failure.
func (r *organizationResource) applyAndWait(ctx context.Context, plan *organizationModel, timeout time.Duration, op string, diags *diag.Diagnostics, save func(*organizationModel) diag.Diagnostics) {
	ns, key := r.data.namespace, plan.Key.ValueString()
	d, err := declarationFromModel(ctx, plan)
	if err != nil {
		diags.AddError("Invalid organisation specification", err.Error())
		return
	}
	m, cdiags := compileDeclaration(ctx, d)
	diags.Append(cdiags...)
	if diags.HasError() {
		return
	}
	obj, err := buildObject(ns, key, m, d)
	if err != nil {
		diags.AddError("Cannot build AgentOrganization", err.Error())
		return
	}
	org, err := r.data.client.Apply(ctx, obj)
	if err != nil {
		addApplyError(diags, ns, key, err)
		return
	}
	tflog.Info(ctx, "applied AgentOrganization", map[string]any{"namespace": ns, "name": key, "generation": org.Generation, "digest": m.Digest})

	plan.ManifestDigest = types.StringValue(m.Digest)
	plan.ResolvedSeats = resolvedSeatsValue(m)
	var waitErr error
	if plan.WaitForReady.ValueBool() {
		var latest *v1alpha1.AgentOrganization
		latest, waitErr = waitForReady(ctx, r.data.client, ns, key)
		if latest != nil {
			org = latest
		}
	}
	setObserved(plan, org, false)
	diags.Append(save(plan)...)
	if waitErr != nil {
		detail := fmt.Sprintf("AgentOrganization %s/%s was written but did not become OperationalReady within %s: %v\n\n"+
			"The resource is recorded in state and nothing was cleaned up. Resolve the blocking condition and run apply again; "+
			"a refresh reports the current conditions.", ns, key, timeout, waitErr)
		if op == "create" {
			detail += " Terraform marks a resource whose creation reported an error as tainted; run `terraform untaint <address>` " +
				"to keep this organisation instead of replacing it on the next apply."
		}
		diags.AddError(fmt.Sprintf("Organisation not ready after %s", op), detail)
	}
}

func (r *organizationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.configured(&resp.Diagnostics) {
		return
	}
	var state organizationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	ns, key := r.data.namespace, state.Key.ValueString()
	org, err := r.data.client.Get(ctx, ns, key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Only a confirmed NotFound removes the resource (§5.6).
			tflog.Warn(ctx, "AgentOrganization not found; removing from state", map[string]any{"namespace": ns, "name": key})
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Cannot read AgentOrganization",
			fmt.Sprintf("%s/%s: %v\n\nThe state is unchanged; only a confirmed not-found response removes the organisation from state.", ns, key, err))
		return
	}
	if mb := org.Annotations[v1alpha1.AnnotationManagedBy]; mb != "" && mb != ManagedByTerraform {
		resp.Diagnostics.AddWarning("Organisation is managed by another owner",
			fmt.Sprintf("AgentOrganization %s/%s is annotated %s=%s. Each organisation must have exactly one declaration owner (§3.1).", ns, key, v1alpha1.AnnotationManagedBy, mb))
	}
	resp.Diagnostics.Append(refresh(ctx, &state, org)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// refresh updates computed values from the live object. Declared values are
// kept: the provider owns them, and any difference between the live object and
// what the provider would write is reported through manifest_digest so that
// the plan shows an update. Status never feeds back into declared values.
func refresh(ctx context.Context, state *organizationModel, org *v1alpha1.AgentOrganization) diag.Diagnostics {
	var diags diag.Diagnostics
	liveDigest := org.Annotations[AnnotationManifestDigest]
	d, err := declarationFromModel(ctx, state)
	if err != nil {
		diags.AddError("Cannot decode state", err.Error())
		return diags
	}
	m, cdiags := compileDeclaration(ctx, d)
	if cdiags.HasError() {
		// The stored declaration no longer compiles (e.g. a newer provider
		// release); report the live digest and let the plan surface errors.
		state.ManifestDigest = types.StringValue(driftDigest(liveDigest, org))
	} else {
		state.ResolvedSeats = resolvedSeatsValue(m)
		switch {
		case liveDigest != m.Digest:
			state.ManifestDigest = types.StringValue(driftDigest(liveDigest, org))
		case !bytes.Equal(canonicalSpecJSON(org.Spec.OrganizationSpec), canonicalSpecJSON(m.Spec)):
			state.ManifestDigest = types.StringValue(driftDigest("", org))
		default:
			state.ManifestDigest = types.StringValue(m.Digest)
		}
	}
	setObserved(state, org, true)
	return diags
}

// driftDigest names the live object's state when it differs from the
// declaration: its recorded digest, or a digest of its live spec when the spec
// was changed outside Terraform.
func driftDigest(liveDigest string, org *v1alpha1.AgentOrganization) string {
	if liveDigest != "" {
		return liveDigest
	}
	sum := sha256.Sum256(canonicalSpecJSON(org.Spec.OrganizationSpec))
	return "drift:sha256:" + hex.EncodeToString(sum[:])
}

func (r *organizationResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.configured(&resp.Diagnostics) {
		return
	}
	var state organizationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	timeout, diags := state.Timeouts.Delete(ctx, defaultTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ns, key := r.data.namespace, state.Key.ValueString()
	if err := r.data.client.Delete(ctx, ns, key); err != nil && !apierrors.IsNotFound(err) {
		resp.Diagnostics.AddError("Cannot delete AgentOrganization", fmt.Sprintf("%s/%s: %v", ns, key, err))
		return
	}
	// The controller's finalizer retires seats according to data_retention.
	var last *v1alpha1.AgentOrganization
	var lastErr error
	for {
		org, err := r.data.client.Get(ctx, ns, key)
		if apierrors.IsNotFound(err) {
			return
		}
		if err == nil {
			last = org
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			detail := fmt.Sprintf("AgentOrganization %s/%s still exists after %s.", ns, key, timeout)
			if last != nil && len(last.Finalizers) > 0 {
				detail += fmt.Sprintf(" Pending finalizers: %s; the controller is retiring seats according to data_retention.", strings.Join(last.Finalizers, ", "))
				if _, blocking := readiness(last); blocking != "" {
					detail += " Status: " + blocking
				}
			}
			if lastErr != nil {
				detail += fmt.Sprintf(" Last error: %v.", lastErr)
			}
			resp.Diagnostics.AddError("Organisation deletion did not complete", detail+" Run destroy again to keep waiting.")
			return
		case <-time.After(pollInterval):
		}
	}
}

// ImportState accepts "namespace/key", or "namespace/key/adopt" to adopt an
// object that carries no managed-by annotation. Objects owned by another
// declaration owner (for example helm) are rejected (§3.1).
func (r *organizationResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if !r.configured(&resp.Diagnostics) {
		return
	}
	parts := strings.Split(req.ID, "/")
	adopt := len(parts) == 3 && parts[2] == "adopt"
	if (len(parts) != 2 && !adopt) || parts[0] == "" || parts[1] == "" {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("expected \"namespace/key\" or \"namespace/key/adopt\", got %q", req.ID))
		return
	}
	ns, key := parts[0], parts[1]
	if ns != r.data.namespace {
		resp.Diagnostics.AddError("Namespace mismatch", fmt.Sprintf("import ID names namespace %q but the provider is configured for namespace %q", ns, r.data.namespace))
		return
	}
	org, err := r.data.client.Get(ctx, ns, key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			resp.Diagnostics.AddError("Organisation not found", fmt.Sprintf("AgentOrganization %s/%s does not exist", ns, key))
		} else {
			resp.Diagnostics.AddError("Cannot read AgentOrganization", fmt.Sprintf("%s/%s: %v", ns, key, err))
		}
		return
	}
	if org.Spec.Key != "" && org.Spec.Key != key {
		resp.Diagnostics.AddError("Organisation identity mismatch", fmt.Sprintf("object %s/%s declares organisation key %q", ns, key, org.Spec.Key))
		return
	}
	switch mb := org.Annotations[v1alpha1.AnnotationManagedBy]; mb {
	case ManagedByTerraform:
	case "":
		if !adopt {
			resp.Diagnostics.AddError("Organisation has no declared owner",
				fmt.Sprintf("AgentOrganization %s/%s has no %s annotation. Adopt it explicitly with import ID %q.", ns, key, v1alpha1.AnnotationManagedBy, ns+"/"+key+"/adopt"))
			return
		}
	default:
		resp.Diagnostics.AddError("Organisation is owned by "+mb,
			fmt.Sprintf("AgentOrganization %s/%s is annotated %s=%s. One owner is chosen per organisation; Terraform will not take over an organisation managed by %s (§3.1).", ns, key, v1alpha1.AnnotationManagedBy, mb, mb))
		return
	}

	d, ok := decodeDeclaration(org.Annotations[AnnotationDeclaration])
	if !ok {
		d, err = declarationFromSpec(org.Spec.OrganizationSpec)
		if err != nil {
			resp.Diagnostics.AddError("Cannot decode organisation", err.Error())
			return
		}
	}
	specType := organizationSchema(ctx).Attributes["spec"].GetType()
	specVal, err := treeToAttr(ctx, specType, d.Spec)
	if err != nil {
		resp.Diagnostics.AddError("Cannot decode organisation specification", err.Error())
		return
	}
	set := func(name string, v any) {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root(name), v)...)
	}
	set("id", observedID(org))
	set("key", key)
	set("display_name", stringOrNull(d.DisplayName))
	set("data_retention", stringOrNull(d.DataRetention))
	set("spec", specVal)
	set("wait_for_ready", true)
}

func (r *organizationResource) configured(diags *diag.Diagnostics) bool {
	if r.data == nil {
		diags.AddError("Provider not configured", "the steadmesh provider was not configured")
		return false
	}
	return true
}

func stringOrNull(v any) types.String {
	if s, ok := v.(string); ok {
		return types.StringValue(s)
	}
	return types.StringNull()
}

// declarationFromModel extracts the declaration from a model.
func declarationFromModel(ctx context.Context, m *organizationModel) (declaration, error) {
	var d declaration
	var err error
	for _, f := range []struct {
		dst *any
		v   attr.Value
	}{{&d.Key, m.Key}, {&d.DisplayName, m.DisplayName}, {&d.DataRetention, m.DataRetention}, {&d.Spec, m.Spec}} {
		if *f.dst, err = attrToTree(ctx, f.v); err != nil {
			return d, err
		}
	}
	return d, nil
}

func compileDeclaration(ctx context.Context, d declaration) (*compile.Manifest, diag.Diagnostics) {
	var diags diag.Diagnostics
	s, err := d.toOrganizationSpec()
	if err != nil {
		diags.AddError("Invalid organisation specification", err.Error())
		return nil, diags
	}
	m, err := compile.Compile(s, compile.DefaultCatalog())
	if err != nil {
		var errs compile.Errors
		if !errors.As(err, &errs) {
			diags.AddError("Invalid organisation specification", err.Error())
			return nil, diags
		}
		specType := organizationSchema(ctx).Attributes["spec"].GetType()
		for _, fe := range errs {
			p := fieldErrorPath(specType, fe.Path)
			diags.AddAttributeError(p, "Invalid organisation specification", fe.Path+": "+fe.Message)
		}
		return nil, diags
	}
	return m, diags
}

// fieldErrorPath maps a compiler path such as
// "seats.reviewer.workspace.shared" or "teams.eng.instruction_refs[1]" to a
// Terraform attribute path, walking the schema type. It stops at the deepest
// segment the schema knows; the diagnostic text always carries the full path.
func fieldErrorPath(specType attr.Type, p string) path.Path {
	segs := strings.Split(p, ".")
	for _, f := range append(topLevelFields, "spec") {
		if segs[0] == f && len(segs) == 1 {
			return path.Root(f)
		}
	}
	cur := path.Root("spec")
	t := specType
	for _, seg := range segs {
		name, idx, hasIdx := splitIndex(seg)
		switch tt := t.(type) {
		case types.ObjectType:
			at, ok := tt.AttrTypes[name]
			if !ok {
				return cur
			}
			cur, t = cur.AtName(name), at
		case types.MapType:
			cur, t = cur.AtMapKey(name), tt.ElemType
		default:
			return cur
		}
		if hasIdx {
			lt, ok := t.(types.ListType)
			if !ok {
				return cur
			}
			cur, t = cur.AtListIndex(idx), lt.ElemType
		}
	}
	return cur
}

func splitIndex(seg string) (string, int, bool) {
	open := strings.IndexByte(seg, '[')
	if open < 0 || !strings.HasSuffix(seg, "]") {
		return seg, 0, false
	}
	var idx int
	if _, err := fmt.Sscanf(seg[open+1:len(seg)-1], "%d", &idx); err != nil {
		return seg, 0, false
	}
	return seg[:open], idx, true
}

func resolvedSeatsValue(m *compile.Manifest) types.Map {
	elemType := types.ObjectType{AttrTypes: resolvedSeatAttrTypes}
	elems := make(map[string]attr.Value, len(m.Seats))
	for k, sm := range m.Seats {
		teams := make([]attr.Value, len(sm.Teams))
		for i, t := range sm.Teams {
			teams[i] = types.StringValue(t)
		}
		elems[k] = types.ObjectValueMust(resolvedSeatAttrTypes, map[string]attr.Value{
			"config_revision": types.StringValue(sm.ConfigRevision),
			"teams":           types.ListValueMust(types.StringType, teams),
			"harness_profile": types.StringValue(sm.HarnessProfile),
			"role_ref":        types.StringValue(sm.RoleRef),
		})
	}
	return types.MapValueMust(elemType, elems)
}

// buildObject renders the AgentOrganization the provider owns: the resolved
// spec plus ownership, digest and declaration annotations.
func buildObject(ns, key string, m *compile.Manifest, d declaration) (map[string]any, error) {
	b, err := json.Marshal(m.Spec)
	if err != nil {
		return nil, err
	}
	var specMap map[string]any
	if err := decodeTree(b, &specMap); err != nil {
		return nil, err
	}
	enc, err := encodeDeclaration(d)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "AgentOrganization",
		"metadata": map[string]any{
			"name":      key,
			"namespace": ns,
			"annotations": map[string]any{
				v1alpha1.AnnotationManagedBy: ManagedByTerraform,
				AnnotationManifestDigest:     m.Digest,
				AnnotationDeclaration:        enc,
			},
		},
		"spec": specMap,
	}, nil
}

func encodeDeclaration(d declaration) (string, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

func decodeDeclaration(s string) (declaration, bool) {
	if s == "" {
		return declaration{}, false
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return declaration{}, false
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return declaration{}, false
	}
	b, err := io.ReadAll(io.LimitReader(zr, 4<<20))
	if err != nil {
		return declaration{}, false
	}
	var d declaration
	if err := decodeTree(b, &d); err != nil {
		return declaration{}, false
	}
	return d, true
}

func addApplyError(diags *diag.Diagnostics, ns, key string, err error) {
	if apierrors.IsConflict(err) {
		diags.AddError("Field ownership conflict on AgentOrganization",
			fmt.Sprintf("Server-side apply of %s/%s by field manager %q conflicts with another manager: %v\n\n"+
				"The provider never forces ownership (§3.1, §5.5). Inspect the owners with "+
				"`kubectl get agentorganization %s -n %s --show-managed-fields -o yaml`, remove the other writer's ownership of these fields, then apply again.",
				ns, key, FieldManager, err, key, ns))
		return
	}
	diags.AddError("Cannot apply AgentOrganization", fmt.Sprintf("%s/%s: %v", ns, key, err))
}

func observedID(org *v1alpha1.AgentOrganization) string {
	if org.Status.OrganizationID != "" {
		return org.Status.OrganizationID
	}
	return string(org.UID)
}

// setObserved copies observed status into computed attributes. The id stays
// stable once known; on refresh, a UID placeholder is upgraded to the
// platform's organisation ID once the controller has assigned one.
func setObserved(m *organizationModel, org *v1alpha1.AgentOrganization, refreshing bool) {
	cur := m.ID.ValueString()
	switch {
	case m.ID.IsUnknown() || m.ID.IsNull() || cur == "":
		m.ID = types.StringValue(observedID(org))
	case refreshing && cur == string(org.UID) && org.Status.OrganizationID != "":
		m.ID = types.StringValue(org.Status.OrganizationID)
	}
	m.EffectiveRevision = types.StringValue(org.Status.EffectiveRevision)
	m.Conditions = conditionsValue(org)
	m.ConnectionDetails = connectionDetailsValue(org.Status.ConnectionDetails)
}

func conditionsValue(org *v1alpha1.AgentOrganization) types.List {
	elemType := types.ObjectType{AttrTypes: conditionAttrTypes}
	conds := append([]metav1Condition(nil), org.Status.Conditions...)
	sortConditions(conds)
	elems := make([]attr.Value, len(conds))
	for i, c := range conds {
		elems[i] = types.ObjectValueMust(conditionAttrTypes, map[string]attr.Value{
			"type":    types.StringValue(c.Type),
			"status":  types.StringValue(string(c.Status)),
			"reason":  types.StringValue(c.Reason),
			"message": types.StringValue(c.Message),
		})
	}
	return types.ListValueMust(elemType, elems)
}

func connectionDetailsValue(cd *v1alpha1.ConnectionDetails) types.Object {
	if cd == nil {
		return types.ObjectNull(connectionDetailsAttrTypes)
	}
	repType := types.ObjectType{AttrTypes: representativeAttrTypes}
	reps := make(map[string]attr.Value, len(cd.Representatives))
	for k, r := range cd.Representatives {
		reps[k] = types.ObjectValueMust(representativeAttrTypes, map[string]attr.Value{
			"seat":             types.StringValue(r.Seat),
			"seat_id":          types.StringValue(r.SeatID),
			"connection":       types.StringValue(r.Connection),
			"adapter":          types.StringValue(r.Adapter),
			"external_user_id": types.StringValue(r.ExternalUserID),
			"mode":             types.StringValue(r.Mode),
		})
	}
	return types.ObjectValueMust(connectionDetailsAttrTypes, map[string]attr.Value{
		"organization_id": types.StringValue(cd.OrganizationID),
		"namespace":       types.StringValue(cd.Namespace),
		"status_endpoint": types.StringValue(cd.StatusEndpoint),
		"representatives": types.MapValueMust(repType, reps),
	})
}
