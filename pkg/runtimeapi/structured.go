package runtimeapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
)

const (
	MaxOutputSchemaBytes    = 16 << 10
	MaxStructuredDepth      = 16
	MaxSchemaNodes          = 256
	StructuredOutputInvalid = "structured_output_invalid"
)

// ValidateOutputSchema accepts a bounded, reference-free JSON Schema subset.
// Unsupported keywords fail closed, rather than being silently ignored. No
// loader is configured, so validation can never access network or local files.
func ValidateOutputSchema(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	_, err := resolveOutputSchema(raw)
	return err
}

func resolveOutputSchema(raw json.RawMessage) (*jsonschema.Resolved, error) {
	v, err := decodeBoundedJSON(raw, MaxOutputSchemaBytes)
	if err != nil {
		return nil, fmt.Errorf("output_schema: %w", err)
	}
	nodes := 0
	if err := checkSchema(v, &nodes); err != nil {
		return nil, fmt.Errorf("output_schema: %w", err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, fmt.Errorf("output_schema: %w", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("output_schema: %w", err)
	}
	return resolved, nil
}

func checkSchema(v any, nodes *int) error {
	*nodes++
	if *nodes > MaxSchemaNodes {
		return fmt.Errorf("exceeds %d schema nodes", MaxSchemaNodes)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("each schema must be an object")
	}
	for key, val := range m {
		switch key {
		case "type", "title", "description":
			if _, ok := val.(string); !ok {
				return fmt.Errorf("%s must be a string", key)
			}
		case "properties":
			props, ok := val.(map[string]any)
			if !ok {
				return fmt.Errorf("properties must be an object")
			}
			for _, child := range props {
				if err := checkSchema(child, nodes); err != nil {
					return err
				}
			}
		case "items":
			if err := checkSchema(val, nodes); err != nil {
				return err
			}
		case "additionalProperties":
			if _, ok := val.(bool); !ok {
				return fmt.Errorf("additionalProperties must be a boolean")
			}
		case "required":
			names, ok := val.([]any)
			if !ok {
				return fmt.Errorf("required must be an array")
			}
			seen := map[string]bool{}
			for _, name := range names {
				s, ok := name.(string)
				if !ok || seen[s] {
					return fmt.Errorf("required must contain unique strings")
				}
				seen[s] = true
			}
		case "minLength", "maxLength", "minItems", "maxItems":
			n, ok := val.(float64)
			if !ok || n < 0 || math.Trunc(n) != n {
				return fmt.Errorf("%s must be a nonnegative integer", key)
			}
		case "minimum", "maximum":
			if _, ok := val.(float64); !ok {
				return fmt.Errorf("%s must be a number", key)
			}
		case "enum":
			values, ok := val.([]any)
			if !ok || len(values) == 0 || len(values) > 64 {
				return fmt.Errorf("enum requires 1 to 64 scalar values")
			}
			seen := map[string]bool{}
			for _, x := range values {
				b, _ := json.Marshal(x)
				if seen[string(b)] {
					return fmt.Errorf("enum values must be unique")
				}
				seen[string(b)] = true
				switch x.(type) {
				case map[string]any, []any:
					return fmt.Errorf("enum supports scalar values only")
				}
			}
		default:
			return fmt.Errorf("unsupported keyword %q", key)
		}
	}
	// Requiring one concrete type prevents accidental unconstrained schemas and
	// keeps the accepted contract explicit; use enum including null only with
	// the null type. Unions and recursive references are deliberately excluded.
	typ, ok := m["type"].(string)
	if !ok {
		return fmt.Errorf("type must be one concrete JSON type")
	}
	switch typ {
	case "object", "array", "string", "number", "integer", "boolean", "null":
	default:
		return fmt.Errorf("unsupported type %q", typ)
	}
	return nil
}

// ValidateStructuredOutput validates exactly one JSON value. It does not extract
// JSON from prose or fences, coerce values, truncate bytes, retry the agent, or
// treat valid output as business approval. Returned bytes preserve the value.
func ValidateStructuredOutput(schema, raw json.RawMessage) (json.RawMessage, error) {
	if len(schema) == 0 {
		return nil, fmt.Errorf("output_schema is required")
	}
	resolved, err := resolveOutputSchema(schema)
	if err != nil {
		return nil, err
	}
	v, err := decodeBoundedJSON(raw, MaxTaskOutputBytes)
	if err != nil {
		return nil, err
	}
	if err := resolved.Validate(v); err != nil {
		return nil, fmt.Errorf("schema mismatch: %w", err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), compact.Bytes()...), nil
}

// ValidateTaskStructuredResult is repeated at the server trust boundary. A
// compromised worker cannot claim successful validation of a different result.
func ValidateTaskStructuredResult(req TaskRunRequest, finish TaskRunFinish) error {
	raw := finish.Result.StructuredOutput
	if len(req.OutputSchema) == 0 {
		if len(raw) > 0 {
			return fmt.Errorf("unsolicited structured_output")
		}
		return nil
	}
	if finish.State != TaskCompleted {
		if len(raw) > 0 {
			return fmt.Errorf("failed task cannot return validated structured_output")
		}
		return nil
	}
	validated, err := ValidateStructuredOutput(req.OutputSchema, raw)
	if err != nil {
		return err
	}
	// The text and machine-readable result must not disagree. Compare parsed
	// compact JSON bytes; the worker emits the validated value into both fields.
	var text bytes.Buffer
	if err := json.Compact(&text, []byte(finish.Result.Output)); err != nil || !bytes.Equal(text.Bytes(), validated) {
		return fmt.Errorf("output and structured_output differ")
	}
	return nil
}

// decodeBoundedJSON rejects duplicate object keys, excessive nesting, trailing
// values and non-UTF-8 input. Numbers follow binary64 JSON validation semantics;
// integers beyond the exact binary64 range are rejected instead of rounded.
func decodeBoundedJSON(raw []byte, maxBytes int) (any, error) {
	if len(raw) == 0 || len(raw) > maxBytes || !utf8.Valid(raw) {
		return nil, fmt.Errorf("JSON must be UTF-8 and between 1 and %d bytes", maxBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var read func(int) (any, error)
	read = func(depth int) (any, error) {
		if depth > MaxStructuredDepth {
			return nil, fmt.Errorf("JSON exceeds depth %d", MaxStructuredDepth)
		}
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := t.(type) {
		case json.Delim:
			switch t {
			case '{':
				obj := map[string]any{}
				for dec.More() {
					key, err := dec.Token()
					if err != nil {
						return nil, err
					}
					s, ok := key.(string)
					if !ok {
						return nil, fmt.Errorf("object key must be a string")
					}
					if _, exists := obj[s]; exists {
						return nil, fmt.Errorf("duplicate object key %q", s)
					}
					val, err := read(depth + 1)
					if err != nil {
						return nil, err
					}
					obj[s] = val
				}
				if _, err := dec.Token(); err != nil {
					return nil, err
				}
				return obj, nil
			case '[':
				arr := []any{}
				for dec.More() {
					val, err := read(depth + 1)
					if err != nil {
						return nil, err
					}
					arr = append(arr, val)
				}
				if _, err := dec.Token(); err != nil {
					return nil, err
				}
				return arr, nil
			default:
				return nil, fmt.Errorf("unexpected delimiter")
			}
		case json.Number:
			// Bound rational parsing too: a short exponent like 1e9999999 must
			// never trigger unbounded big-integer allocation.
			token := t.String()
			if len(token) > 128 {
				return nil, fmt.Errorf("number token exceeds 128 bytes")
			}
			if i := strings.IndexAny(token, "eE"); i >= 0 {
				exponent, err := strconv.Atoi(token[i+1:])
				if err != nil || exponent < -308 || exponent > 308 {
					return nil, fmt.Errorf("number exponent outside supported range")
				}
			}
			n, err := t.Float64()
			if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || math.Abs(n) > 9007199254740991 {
				return nil, fmt.Errorf("number exceeds safe binary64 range")
			}
			exact, ok := new(big.Rat).SetString(token)
			if !ok || (exact.Sign() != 0 && n == 0) || (math.Trunc(n) == n && !exact.IsInt()) {
				return nil, fmt.Errorf("number loses its integer/fractional distinction in binary64")
			}
			return n, nil
		default:
			return t, nil
		}
	}
	v, err := read(0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("expected exactly one JSON value")
	}
	return v, nil
}
