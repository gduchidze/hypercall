// See LICENSE for applicable terms.
package eval

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

func render(v any, vars map[string]any) any {
	switch x := v.(type) {
	case string:
		return interpolation.ReplaceAllStringFunc(x, func(m string) string { key := interpolation.FindStringSubmatch(m)[1]; return scalar(vars[key]) })
	case map[string]any:
		out := map[string]any{}
		for k, v := range x {
			out[k] = render(v, vars)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = render(v, vars)
		}
		return out
	default:
		return x
	}
}
func scalar(v any) string {
	if v == nil {
		return "null"
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, e := json.Marshal(v)
	if e == nil {
		return string(b)
	}
	return fmt.Sprint(v)
}
func assert(output string, a Assertion, vars map[string]any, measurements *Measurements) (AssertionResult, error) {
	value := render(a.Value, vars)
	pass := false
	typ := strings.TrimPrefix(a.Type, "not-")
	inverse := typ != a.Type
	switch typ {
	case "equals":
		switch value.(type) {
		case map[string]any, []any, nil:
			var actual, expected any
			b, _ := json.Marshal(value)
			if json.Unmarshal([]byte(output), &actual) == nil && json.Unmarshal(b, &expected) == nil {
				pass = reflect.DeepEqual(actual, expected)
			}
		default:
			pass = output == scalar(value)
		}
	case "contains":
		pass = strings.Contains(output, scalar(value))
	case "is-json":
		pass = json.Valid([]byte(output))
		if pass && value != nil {
			var parsed any
			_ = json.Unmarshal([]byte(output), &parsed)
			pass = schemaMatches(parsed, value.(map[string]any))
		}
	case "latency":
		if measurements == nil || measurements.LatencyMS == nil {
			return AssertionResult{}, fmt.Errorf("latency unavailable")
		}
		pass = float64(*measurements.LatencyMS) <= *a.Threshold
	case "cost":
		if measurements == nil || measurements.Cost == nil {
			return AssertionResult{}, fmt.Errorf("cost unavailable")
		}
		pass = float64(measurements.Cost.MicroUSD)/1e6 <= *a.Threshold
	default:
		return AssertionResult{}, fmt.Errorf("unsupported assertion %q", a.Type)
	}
	if inverse {
		pass = !pass
	}
	score := 0.0
	if pass {
		score = 1
	}
	if a.Weight != nil && *a.Weight == 0 {
		pass = true
	}
	reason := "Assertion passed"
	if !pass {
		reason = "Assertion failed: " + a.Type
	}
	return AssertionResult{Type: a.Type, Pass: pass, Score: score, Reason: reason}, nil
}
