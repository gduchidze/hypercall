// Package eval compiles immutable evaluation matrices and scores recorded evidence.
// See LICENSE for applicable terms.
package eval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type Candidate struct {
	ID          string `json:"id" yaml:"id"`
	ModelDigest string `json:"model_digest" yaml:"model_digest"`
}
type Assertion struct {
	Threshold        *float64 `json:"threshold,omitempty" yaml:"threshold,omitempty"`
	valuePresent     bool
	ScorerDigest     string            `json:"scorer_digest,omitempty" yaml:"scorer_digest,omitempty"`
	RubricDigest     string            `json:"rubric_digest,omitempty" yaml:"rubric_digest,omitempty"`
	JudgeModelDigest string            `json:"judge_model_digest,omitempty" yaml:"judge_model_digest,omitempty"`
	Parameters       map[string]string `json:"parameters,omitempty" yaml:"parameters,omitempty"`
	Type             string            `json:"type" yaml:"type"`
	Value            any               `json:"value,omitempty" yaml:"value,omitempty"`
	Weight           *float64          `json:"weight,omitempty" yaml:"weight,omitempty"`
}
type Case struct {
	ID         string         `json:"id" yaml:"id"`
	Vars       map[string]any `json:"vars,omitempty" yaml:"vars,omitempty"`
	Assertions []Assertion    `json:"assert" yaml:"assert"`
	Threshold  *float64       `json:"threshold,omitempty" yaml:"threshold,omitempty"`
}
type Config struct {
	OutputSelector   string        `json:"output_selector,omitempty" yaml:"output_selector,omitempty"`
	BaselineReportID string        `json:"baseline_report_id,omitempty" yaml:"baseline_report_id,omitempty"`
	Policy           Policy        `json:"policy" yaml:"policy"`
	SharedPrefix     *SharedPrefix `json:"shared_prefix,omitempty" yaml:"shared_prefix,omitempty"`
	CachePolicy      string        `json:"cache_policy,omitempty" yaml:"cache_policy,omitempty"`
	APIVersion       string        `json:"apiVersion" yaml:"apiVersion"`
	Scope            string        `json:"scope" yaml:"scope"`
	WorkloadDigest   string        `json:"workload_digest" yaml:"workload_digest"`
	Candidates       []Candidate   `json:"candidates" yaml:"candidates"`
	Cases            []Case        `json:"cases" yaml:"cases"`
	Repetitions      int           `json:"repetitions" yaml:"repetitions"`
}
type Cell struct {
	ID          string         `json:"id"`
	CandidateID string         `json:"candidate_id"`
	ModelDigest string         `json:"model_digest"`
	CaseID      string         `json:"case_id"`
	Repetition  int            `json:"repetition"`
	Vars        map[string]any `json:"vars"`
	Assertions  []Assertion    `json:"assert"`
	Threshold   *float64       `json:"threshold,omitempty"`
}
type Plan struct {
	EvidenceDigest string  `json:"evidence_digest,omitempty"`
	Baseline       *Report `json:"baseline,omitempty"`
	Config         Config  `json:"config"`
	Digest         string  `json:"digest"`
	Cells          []Cell  `json:"cells"`
}
type Observation struct {
	Fixture        bool          `json:"fixture,omitempty"`
	AdapterVersion string        `json:"adapter_version,omitempty"`
	Measurements   *Measurements `json:"measurements,omitempty"`
	BranchID       string        `json:"branch_id,omitempty"`
	CallID         string        `json:"call_id,omitempty"`
	CellID         string        `json:"cell_id"`
	Output         *string       `json:"output,omitempty"`
	State          string        `json:"state"`
	Source         string        `json:"source"`
	TraceComplete  bool          `json:"trace_complete"`
}
type AssertionResult struct {
	ReasonTruncated bool         `json:"reason_truncated,omitempty"`
	ErrorCode       string       `json:"error_code,omitempty"`
	Cost            *GradingCost `json:"cost,omitempty"`
	Type            string       `json:"type"`
	Pass            bool         `json:"pass"`
	Score           float64      `json:"score"`
	Reason          string       `json:"reason"`
}
type CellResult struct {
	Fixture        bool              `json:"fixture"`
	AdapterVersion string            `json:"adapter_version,omitempty"`
	Measurements   *Measurements     `json:"measurements,omitempty"`
	Source         string            `json:"source"`
	BranchID       string            `json:"branch_id,omitempty"`
	CallID         string            `json:"call_id,omitempty"`
	CellID         string            `json:"cell_id"`
	CandidateID    string            `json:"candidate_id"`
	State          string            `json:"state"`
	Pass           bool              `json:"pass"`
	Score          float64           `json:"score"`
	Assertions     []AssertionResult `json:"assertions"`
}
type Decision struct {
	Pass    bool     `json:"pass"`
	Reasons []string `json:"reasons"`
}
type Report struct {
	OutputSelector        string             `json:"output_selector"`
	EvaluationAttempts    int                `json:"evaluation_attempts"`
	ScoringCostMicroUSD   *int64             `json:"scoring_cost_micro_usd,omitempty"`
	ExecutionCostMicroUSD *int64             `json:"execution_cost_micro_usd,omitempty"`
	CompatibilityDigest   string             `json:"compatibility_digest"`
	BaselineReportID      string             `json:"baseline_report_id,omitempty"`
	SharedPrefix          *SharedPrefix      `json:"shared_prefix,omitempty"`
	WorkloadDigest        string             `json:"workload_digest"`
	Synthetic             bool               `json:"synthetic"`
	PassRate              float64            `json:"pass_rate"`
	ScoreSamples          int                `json:"score_samples"`
	TotalCostMicroUSD     *int64             `json:"total_cost_micro_usd,omitempty"`
	Candidates            []CandidateSummary `json:"candidates"`
	Pareto                []string           `json:"pareto"`
	PlanDigest            string             `json:"plan_digest"`
	Scope                 string             `json:"scope"`
	Expected              int                `json:"expected"`
	Completed             int                `json:"completed"`
	Passed                int                `json:"passed"`
	MeanScore             float64            `json:"mean_score"`
	Cells                 []CellResult       `json:"cells"`
	CI                    Decision           `json:"ci"`
}

// Scorer is the external grading seam. Deterministic assertions need no scorer.
type Scorer interface {
	Score(context.Context, Assertion, string, map[string]any) (AssertionResult, error)
}

func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

func Compile(data []byte) (*Plan, error) {
	if len(data) == 0 || len(data) > 1<<20 {
		return nil, fmt.Errorf("evaluation YAML must contain 1..1048576 bytes")
	}
	var cfg Config
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("evaluation YAML: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("exactly one evaluation document required")
	}
	if cfg.APIVersion != "hypercall.io/eval/v1" || (cfg.Scope != "whole_task" && cfg.Scope != "continuation") {
		return nil, fmt.Errorf("supported apiVersion and explicit experiment scope required")
	}
	if len(cfg.Candidates) == 0 || len(cfg.Cases) == 0 || cfg.Repetitions < 1 {
		return nil, fmt.Errorf("candidates, cases and positive repetitions required")
	}
	if cfg.OutputSelector == "" {
		cfg.OutputSelector = "raw_utf8"
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	p := &Plan{Config: cfg, Digest: digest(cfg)}
	resolvedBytes, totalAssertions := len(data), 0
	for _, candidate := range cfg.Candidates {
		for _, c := range cfg.Cases {
			keys := make([]string, 0, len(c.Vars))
			for k := range c.Vars {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			combinations := []map[string]any{{}}
			for _, k := range keys {
				values := []any{c.Vars[k]}
				if items, ok := c.Vars[k].([]any); ok && len(items) > 0 {
					if _, ok := items[0].(string); ok {
						values = items
					}
				}
				next := []map[string]any{}
				for _, prior := range combinations {
					for _, value := range values {
						if len(next) >= 1000 {
							return nil, fmt.Errorf("variable expansion exceeds 1000 cells")
						}
						v := map[string]any{}
						for key, x := range prior {
							v[key] = x
						}
						v[k] = value
						next = append(next, v)
					}
				}
				combinations = next
			}
			for _, vars := range combinations {
				for repetition := 0; repetition < cfg.Repetitions; repetition++ {
					cell := Cell{CandidateID: candidate.ID, ModelDigest: candidate.ModelDigest, CaseID: c.ID, Repetition: repetition, Vars: vars, Assertions: c.Assertions, Threshold: c.Threshold}
					cell.ID = digest(cell)
					encoded, _ := json.Marshal(cell)
					resolvedBytes += len(encoded)
					totalAssertions += len(cell.Assertions)
					if resolvedBytes > 2<<20 || totalAssertions > 4000 {
						return nil, fmt.Errorf("resolved matrix exceeds 2MiB or 4000 assertion applications")
					}
					p.Cells = append(p.Cells, cell)
					if len(p.Cells) > 1000 {
						return nil, fmt.Errorf("matrix exceeds 1000 cells")
					}
				}
			}
		}
	}
	return p, nil
}

func Evaluate(ctx context.Context, p *Plan, observations []Observation, scorer Scorer) (*Report, error) {
	if err := ValidateObservations(p, observations); err != nil {
		return nil, err
	}
	if p.Config.BaselineReportID != "" && p.Baseline == nil {
		return nil, fmt.Errorf("resolved immutable baseline report required")
	}
	if p.Baseline != nil && p.Baseline.CompatibilityDigest != PlanCompatibilityDigest(p) {
		return nil, fmt.Errorf("baseline report is incompatible with resolved plan evidence")
	}
	if p == nil {
		return nil, fmt.Errorf("compiled plan required")
	}
	byID := map[string]Observation{}
	known := map[string]bool{}
	for _, cell := range p.Cells {
		known[cell.ID] = true
	}
	for _, o := range observations {
		if !known[o.CellID] {
			return nil, fmt.Errorf("unknown observation cell")
		}
		if _, exists := byID[o.CellID]; exists {
			return nil, fmt.Errorf("duplicate observation cell")
		}
		if o.Source != "synthetic" && o.Source != "branch" {
			return nil, fmt.Errorf("explicit evidence source required")
		}
		byID[o.CellID] = o
	}
	r := &Report{PlanDigest: p.Digest, Scope: p.Config.Scope, Expected: len(p.Cells), CI: Decision{Pass: true, Reasons: []string{}}}
	detailBudget := 128 << 10
	for _, cell := range p.Cells {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result := CellResult{CellID: cell.ID, CandidateID: cell.CandidateID, State: "missing", Assertions: []AssertionResult{}}
		o, ok := byID[cell.ID]
		result.Measurements = o.Measurements
		result.Fixture = o.Fixture
		result.AdapterVersion = o.AdapterVersion
		result.Source = o.Source
		result.BranchID = o.BranchID
		result.CallID = o.CallID
		if ok && o.State == "complete" && o.Output != nil && o.TraceComplete {
			result.State = "complete"
			result.Pass = true
			r.Completed++
			var total, weight float64
			scoringError := false
			for _, a := range cell.Assertions {
				var assertion AssertionResult
				var err error
				if a.Type == "python" || a.Type == "llm-judge" {
					if scorer == nil {
						err = fmt.Errorf("scorer unavailable")
					} else {
						assertion, err = scorer.Score(ctx, a, *o.Output, cell.Vars)
					}
				} else {
					assertion, err = assert(*o.Output, a, cell.Vars, o.Measurements)
				}
				if assertion.Cost != nil {
					copy := *assertion.Cost
					copy.SourceDigest = ""
					copy.SourceTruncated = false
					assertion.Cost = &copy
				}
				if c := assertion.Cost; c != nil && (!finite(c.Amount) || c.Amount < 0 || c.Amount*1e6 >= float64(math.MaxInt64) || c.Currency != "USD" || (c.Kind != "synthetic" && c.Kind != "estimated" && c.Kind != "measured") || len(c.Source) == 0 || len(c.Source) > 1024) {
					assertion.Cost = nil
					assertion.ErrorCode = "invalid_cost"
					assertion.Reason = "Scorer returned invalid cost"
				}
				if err != nil || !finite(assertion.Score) || assertion.Score < 0 || assertion.Score > 1 || assertion.ErrorCode != "" {
					scoringError = true
					assertion.Type = a.Type
					assertion.Pass = false
					assertion.Score = 0
					if assertion.ErrorCode == "" {
						assertion.ErrorCode = "evaluation_error"
					}
					if assertion.Reason == "" {
						assertion.Reason = "Scorer evaluation error"
					}
					result.State = "evaluation_error"
				}
				assertion.Type = a.Type
				if len(assertion.ErrorCode) > 64 || strings.Trim(assertion.ErrorCode, "abcdefghijklmnopqrstuvwxyz0123456789_") != "" {
					assertion.ErrorCode = "evaluation_error"
				}
				maximum := 4096
				if len(assertion.Reason) > maximum {
					assertion.Reason = assertion.Reason[:maximum]
					assertion.ReasonTruncated = true
				}
				boundDetails(&assertion, &detailBudget)
				w := 1.0
				if a.Weight != nil {
					w = *a.Weight
				}
				if w == 0 && !scoringError {
					assertion.Pass = true
				}
				total += assertion.Score * w
				weight += w
				result.Pass = result.Pass && assertion.Pass
				result.Assertions = append(result.Assertions, assertion)
			}
			if weight > 0 {
				result.Score = total / weight
			}
			if !finite(total) || !finite(weight) || !finite(result.Score) {
				scoringError = true
				result.State = "evaluation_error"
				result.Score = 0
			}
			if len(cell.Assertions) == 0 {
				result.Score = 1
			}
			if cell.Threshold != nil {
				result.Pass = result.Score >= *cell.Threshold
			}
			if scoringError {
				result.Pass = false
				result.Score = 0
				r.Completed--
			}
		} else if ok {
			result.State = o.State
			if result.State == "complete" {
				result.State = "incomplete"
			}
		}
		if result.Pass {
			r.Passed++
		}
		if result.State == "complete" {
			r.MeanScore += result.Score
		}
		r.Cells = append(r.Cells, result)
	}
	if r.Completed > 0 {
		r.MeanScore /= float64(r.Completed)
	}
	if r.Completed != r.Expected {
		r.CI.Reasons = append(r.CI.Reasons, "incomplete_matrix")
	}
	for _, cell := range r.Cells {
		if cell.State == "execution_error" || cell.State == "evaluation_error" {
			found := false
			for _, reason := range r.CI.Reasons {
				found = found || reason == cell.State
			}
			if !found {
				r.CI.Reasons = append(r.CI.Reasons, cell.State)
			}
		}
	}
	r.CI.Pass = len(r.CI.Reasons) == 0
	finishReport(p, r)
	return r, nil
}

func ValidateObservations(p *Plan, observations []Observation) error {
	if p == nil {
		return fmt.Errorf("compiled plan required")
	}
	known := map[string]bool{}
	for _, cell := range p.Cells {
		known[cell.ID] = true
	}
	seen := map[string]bool{}
	for _, o := range observations {
		if !known[o.CellID] || seen[o.CellID] {
			return fmt.Errorf("unknown or duplicate observation cell")
		}
		seen[o.CellID] = true
		if o.Source != "synthetic" && o.Source != "branch" {
			return fmt.Errorf("explicit evidence source required")
		}
		if o.Output != nil && len(*o.Output) > 1<<20 {
			return fmt.Errorf("output exceeds 1MiB")
		}
		switch o.State {
		case "complete", "execution_error", "evaluation_error", "missing":
		default:
			return fmt.Errorf("unsupported observation state")
		}
		if o.Source == "synthetic" {
			if o.BranchID != "" || o.CallID != "" {
				return fmt.Errorf("synthetic evidence cannot assert branch identity")
			}
			if o.Measurements != nil && o.Measurements.Cost != nil && o.Measurements.Cost.Kind != "synthetic" {
				return fmt.Errorf("synthetic observations require synthetic costs")
			}
		}
		if o.Measurements != nil {
			if o.Measurements.LatencyMS != nil && *o.Measurements.LatencyMS < 0 {
				return fmt.Errorf("negative latency")
			}
			if err := validCost(o.Measurements.Cost); err != nil {
				return err
			}
		}
	}
	b, err := json.Marshal(observations)
	if err != nil || len(b) > 2<<20 {
		return fmt.Errorf("observation input exceeds 2MiB")
	}
	return nil
}
