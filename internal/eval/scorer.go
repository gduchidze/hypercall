package eval

import (
	"context"
	"fmt"

	"github.com/gduchidze/hypercall/internal/scorer"
)

type GradingCost struct {
	SourceDigest    string  `json:"source_digest,omitempty"`
	SourceTruncated bool    `json:"source_truncated,omitempty"`
	Amount          float64 `json:"amount"`
	Currency        string  `json:"currency"`
	Kind            string  `json:"kind"`
	Source          string  `json:"source"`
}

// IsolatedScorer routes only to operator-configured, pinned OCI workers.
type IsolatedScorer map[string]*scorer.Runner

func (s IsolatedScorer) Score(ctx context.Context, a Assertion, output string, vars map[string]any) (AssertionResult, error) {
	runner, ok := s[a.ScorerDigest]
	if !ok || runner == nil {
		err := fmt.Errorf("scorer digest is not configured")
		return AssertionResult{Type: a.Type, ErrorCode: "scorer_not_configured", Reason: err.Error()}, err
	}
	variables := map[string]string{}
	for key, v := range vars {
		variables[key] = scalar(v)
	}
	result, err := runner.Evaluate(ctx, scorer.Request{Output: output, Variables: variables, ScorerDigest: a.ScorerDigest, RubricDigest: a.RubricDigest, JudgeModelDigest: a.JudgeModelDigest, Parameters: a.Parameters})
	out := AssertionResult{Type: a.Type, Pass: result.Pass, Score: result.Score, Reason: result.Reason, ErrorCode: result.ErrorCode}
	if result.Cost != nil {
		c := result.Cost
		out.Cost = &GradingCost{Amount: c.Amount, Currency: c.Currency, Kind: c.Kind, Source: c.Source}
	}
	if err != nil || out.ErrorCode != "" {
		out.Pass = false
		if out.ErrorCode == "" {
			out.ErrorCode = "scorer_error"
		}
		if out.Reason == "" && err != nil {
			out.Reason = err.Error()
		}
		if err != nil {
			return out, fmt.Errorf("isolated scorer %s: %w", out.ErrorCode, err)
		}
		return out, fmt.Errorf("isolated scorer %s: %s", out.ErrorCode, out.Reason)
	}
	return out, nil
}
