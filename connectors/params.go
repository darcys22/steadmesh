package connectors

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// ParamsObject parses exactly one JSON object, rejecting duplicate top-level
// fields. Raw values preserve integers without a float64 round trip when the
// gateway canonicalizes a request for the operation ledger.
func ParamsObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("%w: params must be a JSON object", ErrPermanent)
	}
	p := map[string]json.RawMessage{}
	for d.More() {
		tok, err := d.Token()
		if err != nil {
			return nil, fmt.Errorf("%w: params: %v", ErrPermanent, err)
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("%w: params field must be a string", ErrPermanent)
		}
		if _, exists := p[key]; exists {
			return nil, fmt.Errorf("%w: duplicate params field %q", ErrPermanent, key)
		}
		var v json.RawMessage
		if err := d.Decode(&v); err != nil {
			return nil, fmt.Errorf("%w: params: %v", ErrPermanent, err)
		}
		p[key] = v
	}
	if _, err := d.Token(); err != nil {
		return nil, fmt.Errorf("%w: params: %v", ErrPermanent, err)
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: params: trailing data", ErrPermanent)
	}
	return p, nil
}

// DecodeParams decodes a flat, JSON-tagged parameter struct. Unlike encoding/json
// alone it rejects unknown fields, case aliases and duplicate keys, so execution
// and target authorization cannot interpret different spellings or values.
func DecodeParams(raw json.RawMessage, dst any) error {
	fields, err := ParamsObject(raw)
	if err != nil {
		return err
	}
	t := reflect.TypeOf(dst)
	if t == nil || t.Kind() != reflect.Pointer || t.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("%w: params destination must be a struct pointer", ErrPermanent)
	}
	t = t.Elem()
	allowed := make(map[string]bool, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		name := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			allowed[name] = true
		}
	}
	for key := range fields {
		if !allowed[key] {
			return fmt.Errorf("%w: unknown params field %q", ErrPermanent, key)
		}
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("%w: params: %v", ErrPermanent, err)
	}
	return nil
}
