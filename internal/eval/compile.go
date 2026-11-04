// See LICENSE for applicable terms.
package eval

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var pin = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
var interpolation = regexp.MustCompile(`{{\s*([A-Za-z_][A-Za-z0-9_]*)\s*}}`)

func (a *Assertion) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("assertion must be mapping")
	}
	for i := 0; i < len(node.Content); i += 2 {
		switch node.Content[i].Value {
		case "type", "value", "weight", "threshold", "scorer_digest", "rubric_digest", "judge_model_digest", "parameters":
		default:
			return fmt.Errorf("unsupported assertion field %q", node.Content[i].Value)
		}
		if node.Content[i].Value == "value" {
			a.valuePresent = true
		}
	}
	type plain Assertion
	value := plain(*a)
	if err := node.Decode(&value); err != nil {
		return err
	}
	*a = Assertion(value)
	return nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func validateConfig(c Config) error {
	if len(c.Candidates) > 128 {
		return fmt.Errorf("at most 128 candidates supported")
	}
	if c.OutputSelector != "raw_utf8" {
		return fmt.Errorf("only raw_utf8 inference response selection supported")
	}
	if c.Policy.TimeoutSeconds < 0 || c.Policy.TimeoutSeconds > 3600 {
		return fmt.Errorf("evaluation timeout_seconds must be 0..3600 (0 defaults to 300)")
	}
	if c.Policy.MaxScoreDrop != nil && (!finite(*c.Policy.MaxScoreDrop) || *c.Policy.MaxScoreDrop < 0 || *c.Policy.MaxScoreDrop > 1) {
		return fmt.Errorf("max_score_drop must be 0..1")
	}
	if c.Policy.MaxScoreDrop != nil && c.BaselineReportID == "" {
		return fmt.Errorf("max_score_drop requires baseline_report_id")
	}
	if c.Policy.MinimumSamples < 0 || c.Policy.MinimumSamples > 10000 {
		return fmt.Errorf("minimum_samples must be 0..10000")
	}
	if c.Policy.MaxCostMicroUSD != nil && *c.Policy.MaxCostMicroUSD < 0 {
		return fmt.Errorf("cost budget must be nonnegative")
	}
	if c.Policy.MaxLatencyMS != nil && *c.Policy.MaxLatencyMS < 0 {
		return fmt.Errorf("latency budget must be nonnegative")
	}
	if c.Policy.MinimumPassRate != nil && (!finite(*c.Policy.MinimumPassRate) || *c.Policy.MinimumPassRate < 0 || *c.Policy.MinimumPassRate > 1) {
		return fmt.Errorf("minimum_pass_rate must be in 0..1")
	}
	if c.SharedPrefix != nil {
		if c.Scope != "continuation" || c.SharedPrefix.ID == "" {
			return fmt.Errorf("identified shared prefix requires continuation scope")
		}
		if err := validCost(c.SharedPrefix.Cost); err != nil {
			return err
		}
	}
	if !pin.MatchString(c.WorkloadDigest) {
		return fmt.Errorf("immutable workload_digest required")
	}
	seen := map[string]bool{}
	for _, v := range c.Candidates {
		if !identifier.MatchString(v.ID) || !pin.MatchString(v.ModelDigest) || seen[v.ID] {
			return fmt.Errorf("unique candidate IDs and immutable model digests required")
		}
		seen[v.ID] = true
	}
	seen = map[string]bool{}
	for _, v := range c.Cases {
		if !identifier.MatchString(v.ID) || seen[v.ID] {
			return fmt.Errorf("unique case IDs required")
		}
		seen[v.ID] = true
		if len(v.Assertions) == 0 || len(v.Assertions) > 64 {
			return fmt.Errorf("1..64 assertions per case required")
		}
		if v.Threshold != nil && (!finite(*v.Threshold) || *v.Threshold < 0 || *v.Threshold > 1) {
			return fmt.Errorf("case threshold must be finite in 0..1")
		}
		for key, value := range v.Vars {
			if !identifier.MatchString(key) {
				return fmt.Errorf("invalid variable name")
			}
			if err := validateValue(value, 0); err != nil {
				return err
			}
			if values, ok := value.([]any); ok {
				if len(values) == 0 {
					return fmt.Errorf("empty variable dimensions unsupported")
				}
				for _, value := range values {
					if _, ok := value.(string); !ok {
						return fmt.Errorf("only string-array variable dimensions supported")
					}
				}
			}
			if err := validateTemplates(value, map[string]any{}); err != nil {
				return fmt.Errorf("variable values cannot contain expressions: %w", err)
			}
		}
		for _, a := range v.Assertions {
			if a.Weight != nil && (!finite(*a.Weight) || *a.Weight < 0) {
				return fmt.Errorf("assertion weight must be finite and nonnegative")
			}
			t := strings.TrimPrefix(a.Type, "not-")
			switch t {
			case "equals", "contains", "is-json", "python", "llm-judge", "latency", "cost":
			default:
				return fmt.Errorf("unsupported assertion %q", a.Type)
			}
			if t == "cost" || t == "latency" {
				if a.Threshold == nil || !finite(*a.Threshold) || *a.Threshold < 0 {
					return fmt.Errorf("numeric assertion requires finite nonnegative threshold")
				}
			} else if a.Threshold != nil {
				return fmt.Errorf("assertion threshold only supported for cost/latency")
			}
			if t == "python" || t == "llm-judge" {
				if a.Type != t || !pin.MatchString(a.ScorerDigest) {
					return fmt.Errorf("pinned non-negated scorer required")
				}
				if a.RubricDigest != "" && !pin.MatchString(a.RubricDigest) {
					return fmt.Errorf("invalid rubric digest")
				}
				if t == "llm-judge" && (!pin.MatchString(a.RubricDigest) || !pin.MatchString(a.JudgeModelDigest) || len(a.Parameters) == 0) {
					return fmt.Errorf("judge requires rubric/model digests and parameters")
				}
			}
			if t == "contains" {
				s, ok := a.Value.(string)
				_, numeric := number(a.Value)
				if (!ok || s == "") && !numeric {
					return fmt.Errorf("contains requires nonempty text")
				}
			}
			if t == "equals" && !a.valuePresent {
				return fmt.Errorf("equals requires explicit value (null is supported)")
			}
			if t == "is-json" && a.Value != nil {
				if err := validateSchema(a.Value, 0); err != nil {
					return err
				}
			}
			if err := validateValue(a.Value, 0); err != nil {
				return err
			}
			if err := validateTemplates(a.Value, v.Vars); err != nil {
				return err
			}
		}
	}
	return nil
}
func validateValue(v any, depth int) error {
	if depth > 16 {
		return fmt.Errorf("configuration nesting exceeds 16")
	}
	switch x := v.(type) {
	case map[string]any:
		for _, v := range x {
			if err := validateValue(v, depth+1); err != nil {
				return err
			}
		}
	case []any:
		for _, v := range x {
			if err := validateValue(v, depth+1); err != nil {
				return err
			}
		}
	case float64:
		if !finite(x) {
			return fmt.Errorf("nonfinite number")
		}
	case nil, string, bool, int, int64, uint64, json.Number:
	default:
		return fmt.Errorf("unsupported configuration value")
	}
	return nil
}
func validateTemplates(v any, vars map[string]any) error {
	switch x := v.(type) {
	case string:
		if strings.HasPrefix(x, "file://") {
			return fmt.Errorf("file expressions unsupported")
		}
		for _, m := range interpolation.FindAllStringSubmatch(x, -1) {
			if _, ok := vars[m[1]]; !ok {
				return fmt.Errorf("missing case variable %q", m[1])
			}
		}
		rest := interpolation.ReplaceAllString(x, "")
		if strings.Contains(rest, "{{") || strings.Contains(rest, "}}") || strings.Contains(rest, "{%") {
			return fmt.Errorf("only plain case variable interpolation supported")
		}
	case []any:
		for _, v := range x {
			if err := validateTemplates(v, vars); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, v := range x {
			if err := validateTemplates(v, vars); err != nil {
				return err
			}
		}
	}
	return nil
}
