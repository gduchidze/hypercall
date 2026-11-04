// See LICENSE for applicable terms.
package eval

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
)

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float64:
		return x, finite(x)
	case json.Number:
		n, e := x.Float64()
		return n, e == nil && finite(n)
	}
	return 0, false
}
func validateSchema(value any, depth int) error {
	if depth > 16 {
		return fmt.Errorf("schema nesting exceeds 16")
	}
	s, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("schema must be an object")
	}
	for key, v := range s {
		switch key {
		case "type":
			typ, ok := v.(string)
			if !ok {
				return fmt.Errorf("schema type must be text")
			}
			switch typ {
			case "object", "array", "string", "number", "integer", "boolean", "null":
			default:
				return fmt.Errorf("unsupported schema type")
			}
		case "properties":
			props, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("schema properties must be object")
			}
			for _, child := range props {
				if err := validateSchema(child, depth+1); err != nil {
					return err
				}
			}
		case "items":
			if err := validateSchema(v, depth+1); err != nil {
				return err
			}
		case "required":
			items, ok := v.([]any)
			if !ok {
				return fmt.Errorf("required must be array")
			}
			for _, item := range items {
				if _, ok := item.(string); !ok {
					return fmt.Errorf("required must contain text")
				}
			}
		case "additionalProperties":
			if _, ok := v.(bool); !ok {
				return fmt.Errorf("additionalProperties must be boolean")
			}
		case "enum":
			if _, ok := v.([]any); !ok {
				return fmt.Errorf("enum must be array")
			}
		case "minimum", "maximum":
			if _, ok := number(v); !ok {
				return fmt.Errorf("schema numeric bound required")
			}
		default:
			return fmt.Errorf("unsupported schema keyword %q", key)
		}
	}
	return nil
}
func schemaMatches(value any, s map[string]any) bool {
	if typ, ok := s["type"].(string); ok {
		valid := false
		switch typ {
		case "object":
			_, valid = value.(map[string]any)
		case "array":
			_, valid = value.([]any)
		case "string":
			_, valid = value.(string)
		case "boolean":
			_, valid = value.(bool)
		case "null":
			valid = value == nil
		case "number":
			_, valid = number(value)
		case "integer":
			n, ok := number(value)
			valid = ok && math.Trunc(n) == n
		}
		if !valid {
			return false
		}
	}
	if values, ok := s["enum"].([]any); ok {
		found := false
		for _, expected := range values {
			a, _ := json.Marshal(value)
			b, _ := json.Marshal(expected)
			var x, y any
			_ = json.Unmarshal(a, &x)
			_ = json.Unmarshal(b, &y)
			found = found || reflect.DeepEqual(x, y)
		}
		if !found {
			return false
		}
	}
	if n, ok := number(value); ok {
		if bound, exists := s["minimum"]; exists {
			b, _ := number(bound)
			if n < b {
				return false
			}
		}
		if bound, exists := s["maximum"]; exists {
			b, _ := number(bound)
			if n > b {
				return false
			}
		}
	}
	if obj, ok := value.(map[string]any); ok {
		if required, ok := s["required"].([]any); ok {
			for _, key := range required {
				if _, exists := obj[key.(string)]; !exists {
					return false
				}
			}
		}
		props, _ := s["properties"].(map[string]any)
		for k, v := range obj {
			if child, exists := props[k]; exists {
				if !schemaMatches(v, child.(map[string]any)) {
					return false
				}
			} else if additional, ok := s["additionalProperties"].(bool); ok && !additional {
				return false
			}
		}
	}
	if items, ok := value.([]any); ok {
		if child, exists := s["items"]; exists {
			for _, v := range items {
				if !schemaMatches(v, child.(map[string]any)) {
					return false
				}
			}
		}
	}
	return true
}
