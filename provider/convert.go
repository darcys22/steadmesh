package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/darcys22/steadmesh/pkg/spec"
)

// The Terraform schema of `spec` uses the same snake_case names as the JSON
// tags of pkg/spec.OrganizationSpec. Conversion therefore goes through a
// generic tree (map[string]any, []any, string, bool, int64, nil) rather than
// hand-written per-field code: the schema and the spec cannot silently drift,
// because decoding uses DisallowUnknownFields and TestSchemaMirrorsSpec checks
// the reverse direction.

// errUnknown reports that a value is not yet known (plan time).
var errUnknown = errors.New("value is unknown")

// topLevelFields are spec.OrganizationSpec fields exposed as resource
// attributes rather than inside `spec`.
var topLevelFields = []string{"key", "display_name", "data_retention"}

// valueToTree converts a Terraform value into a generic tree. Nulls become nil
// and are kept as explicit entries in objects, so that a tree round-trips back
// to the identical Terraform value.
func valueToTree(v tftypes.Value) (any, error) {
	if !v.IsKnown() {
		return nil, errUnknown
	}
	if v.IsNull() {
		return nil, nil
	}
	t := v.Type()
	switch {
	case t.Is(tftypes.String):
		var s string
		err := v.As(&s)
		return s, err
	case t.Is(tftypes.Bool):
		var b bool
		err := v.As(&b)
		return b, err
	case t.Is(tftypes.Number):
		var f big.Float
		if err := v.As(&f); err != nil {
			return nil, err
		}
		i, acc := f.Int64()
		if acc != big.Exact {
			return nil, fmt.Errorf("number %s is not an integer", f.String())
		}
		return i, nil
	case t.Is(tftypes.List{}), t.Is(tftypes.Set{}), t.Is(tftypes.Tuple{}):
		var elems []tftypes.Value
		if err := v.As(&elems); err != nil {
			return nil, err
		}
		out := make([]any, len(elems))
		for i, e := range elems {
			x, err := valueToTree(e)
			if err != nil {
				return nil, err
			}
			out[i] = x
		}
		return out, nil
	case t.Is(tftypes.Map{}), t.Is(tftypes.Object{}):
		var elems map[string]tftypes.Value
		if err := v.As(&elems); err != nil {
			return nil, err
		}
		out := make(map[string]any, len(elems))
		for k, e := range elems {
			x, err := valueToTree(e)
			if err != nil {
				return nil, err
			}
			out[k] = x
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported type %s", t)
}

// treeToValue converts a generic tree into a Terraform value of type t.
// Missing object attributes become null.
func treeToValue(t tftypes.Type, v any) (tftypes.Value, error) {
	if v == nil {
		return tftypes.NewValue(t, nil), nil
	}
	switch tt := t.(type) {
	case tftypes.Object:
		m, ok := v.(map[string]any)
		if !ok {
			return tftypes.Value{}, fmt.Errorf("expected object, got %T", v)
		}
		for k := range m {
			if _, ok := tt.AttributeTypes[k]; !ok {
				return tftypes.Value{}, fmt.Errorf("unknown attribute %q", k)
			}
		}
		vals := make(map[string]tftypes.Value, len(tt.AttributeTypes))
		for name, at := range tt.AttributeTypes {
			x, err := treeToValue(at, m[name])
			if err != nil {
				return tftypes.Value{}, fmt.Errorf("%s: %w", name, err)
			}
			vals[name] = x
		}
		return tftypes.NewValue(tt, vals), nil
	case tftypes.Map:
		m, ok := v.(map[string]any)
		if !ok {
			return tftypes.Value{}, fmt.Errorf("expected map, got %T", v)
		}
		vals := make(map[string]tftypes.Value, len(m))
		for k, e := range m {
			x, err := treeToValue(tt.ElementType, e)
			if err != nil {
				return tftypes.Value{}, fmt.Errorf("%s: %w", k, err)
			}
			vals[k] = x
		}
		return tftypes.NewValue(tt, vals), nil
	case tftypes.List:
		l, ok := v.([]any)
		if !ok {
			return tftypes.Value{}, fmt.Errorf("expected list, got %T", v)
		}
		vals := make([]tftypes.Value, len(l))
		for i, e := range l {
			x, err := treeToValue(tt.ElementType, e)
			if err != nil {
				return tftypes.Value{}, fmt.Errorf("[%d]: %w", i, err)
			}
			vals[i] = x
		}
		return tftypes.NewValue(tt, vals), nil
	}
	switch {
	case t.Is(tftypes.String):
		s, ok := v.(string)
		if !ok {
			return tftypes.Value{}, fmt.Errorf("expected string, got %T", v)
		}
		return tftypes.NewValue(t, s), nil
	case t.Is(tftypes.Bool):
		b, ok := v.(bool)
		if !ok {
			return tftypes.Value{}, fmt.Errorf("expected bool, got %T", v)
		}
		return tftypes.NewValue(t, b), nil
	case t.Is(tftypes.Number):
		switch n := v.(type) {
		case int64:
			return tftypes.NewValue(t, new(big.Float).SetInt64(n)), nil
		case int:
			return tftypes.NewValue(t, new(big.Float).SetInt64(int64(n))), nil
		case float64:
			return tftypes.NewValue(t, big.NewFloat(n)), nil
		case json.Number:
			f, _, err := big.ParseFloat(n.String(), 10, 512, big.ToNearestEven)
			if err != nil {
				return tftypes.Value{}, err
			}
			return tftypes.NewValue(t, f), nil
		}
		return tftypes.Value{}, fmt.Errorf("expected number, got %T", v)
	}
	return tftypes.Value{}, fmt.Errorf("unsupported type %s", t)
}

// attrToTree converts a framework value into a tree.
func attrToTree(ctx context.Context, v attr.Value) (any, error) {
	tv, err := v.ToTerraformValue(ctx)
	if err != nil {
		return nil, err
	}
	return valueToTree(tv)
}

// treeToAttr converts a tree into a framework value of type t.
func treeToAttr(ctx context.Context, t attr.Type, v any) (attr.Value, error) {
	tv, err := treeToValue(t.TerraformType(ctx), v)
	if err != nil {
		return nil, err
	}
	return t.ValueFromTerraform(ctx, tv)
}

// declaration is the declared (unresolved) organisation as written in
// Terraform: the top-level attributes plus the `spec` tree.
type declaration struct {
	Key           any `json:"key"`
	DisplayName   any `json:"display_name"`
	DataRetention any `json:"data_retention"`
	Spec          any `json:"spec"`
}

// toOrganizationSpec builds the spec the compiler consumes. Unknown fields are
// rejected so that schema/spec drift is caught by tests.
func (d declaration) toOrganizationSpec() (spec.OrganizationSpec, error) {
	flat := map[string]any{}
	if m, ok := d.Spec.(map[string]any); ok {
		for k, v := range m {
			flat[k] = v
		}
	} else if d.Spec != nil {
		return spec.OrganizationSpec{}, fmt.Errorf("spec must be an object")
	}
	flat["key"] = d.Key
	flat["display_name"] = d.DisplayName
	flat["data_retention"] = d.DataRetention
	b, err := json.Marshal(flat)
	if err != nil {
		return spec.OrganizationSpec{}, err
	}
	var out spec.OrganizationSpec
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return spec.OrganizationSpec{}, err
	}
	return out, nil
}

// declarationFromSpec is the inverse used when no declared tree is available
// (adoption of an object Terraform did not create). Zero values are omitted,
// matching the omitempty JSON encoding of the spec.
func declarationFromSpec(s spec.OrganizationSpec) (declaration, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return declaration{}, err
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return declaration{}, err
	}
	d := declaration{Key: m["key"], DisplayName: m["display_name"], DataRetention: m["data_retention"]}
	for _, k := range topLevelFields {
		delete(m, k)
	}
	d.Spec = m
	return d, nil
}

// decodeTree parses JSON into a tree, keeping integers exact.
func decodeTree(b []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	return dec.Decode(out)
}

// canonicalSpecJSON returns a deterministic encoding of a spec. encoding/json
// sorts map keys, so equal specs produce equal bytes.
func canonicalSpecJSON(s spec.OrganizationSpec) []byte {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return b
}

func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
