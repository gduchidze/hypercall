// See LICENSE for applicable terms.
package eval_test

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"testing"

	"github.com/gduchidze/hypercall/internal/eval"
)

type fixtureGrade struct {
	Pass       bool           `json:"pass"`
	Score      float64        `json:"score"`
	Components []fixtureGrade `json:"components"`
	Error      string         `json:"error"`
}
type fixtureAssertion struct {
	Measurements *eval.Measurements `json:"measurements"`
	ID           string             `json:"id"`
	Output       string             `json:"output"`
	Vars         map[string]any     `json:"vars"`
	Assertions   []json.RawMessage  `json:"assertions"`
	Threshold    *float64           `json:"threshold"`
	Reject       bool               `json:"reject"`
	Expected     fixtureGrade       `json:"expected"`
}
type fixtureCell struct {
	Candidate  string         `json:"candidate"`
	Vars       map[string]any `json:"vars"`
	Repetition int            `json:"repetition"`
	Output     *string        `json:"output"`
	Pass       bool           `json:"pass"`
	Score      float64        `json:"score"`
	Error      bool           `json:"error"`
}
type fixtureMatrix struct {
	Reject      bool              `json:"reject"`
	ID          string            `json:"id"`
	Vars        map[string]any    `json:"vars"`
	Assertions  []json.RawMessage `json:"assertions"`
	Candidates  []string          `json:"candidates"`
	Repetitions int               `json:"repetitions"`
	Expected    struct {
		Cells     []fixtureCell `json:"cells"`
		Passed    int           `json:"passed"`
		MeanScore float64       `json:"mean_score"`
	} `json:"expected"`
}

func fixtureConfig(assertions []json.RawMessage, vars map[string]any, threshold *float64, candidates []string, repetitions int) []byte {
	cc := []map[string]any{}
	for _, id := range candidates {
		cc = append(cc, map[string]any{"id": id, "model_digest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"})
	}
	c := map[string]any{"id": "fixture", "vars": vars, "assert": assertions}
	if threshold != nil {
		c["threshold"] = *threshold
	}
	b, _ := json.Marshal(map[string]any{"apiVersion": "hypercall.io/eval/v1", "scope": "whole_task", "workload_digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "candidates": cc, "cases": []any{c}, "repetitions": repetitions})
	return b
}

func TestEvaluationAssertionConformance(t *testing.T) {
	data, err := os.ReadFile("../../test/fixtures/evaluation/expected.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		SchemaVersion string             `json:"schema_version"`
		Assertions    []fixtureAssertion `json:"assertions"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.SchemaVersion != "evaluation-fixtures.v1" || len(fixture.Assertions) == 0 {
		t.Fatal("missing evaluation contract fixtures")
	}
	for _, sample := range fixture.Assertions {
		t.Run(sample.ID, func(t *testing.T) {
			plan, err := eval.Compile(fixtureConfig(sample.Assertions, sample.Vars, sample.Threshold, []string{"fixture"}, 1))
			if sample.Reject {
				if err == nil {
					t.Fatal("unsupported evaluation input admitted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}

			report, err := eval.Evaluate(context.Background(), plan, []eval.Observation{{CellID: plan.Cells[0].ID, Output: &sample.Output, Measurements: sample.Measurements, State: "complete", Source: "synthetic", TraceComplete: true}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := report.Cells[0]
			if sample.Expected.Error != "" {
				if got.State != "evaluation_error" || got.Pass || report.CI.Pass {
					t.Fatalf("recorded runtime error became success: %+v", report)
				}
				return
			}
			if got.Pass != sample.Expected.Pass || !sameScore(got.Score, sample.Expected.Score) {
				t.Fatalf("expected pass=%v score=%g; Hypercall pass=%v score=%g", sample.Expected.Pass, sample.Expected.Score, got.Pass, got.Score)
			}
			if len(got.Assertions) != len(sample.Expected.Components) {
				t.Fatal("component count differs")
			}
			for i, want := range sample.Expected.Components {
				if got.Assertions[i].Pass != want.Pass || !sameScore(got.Assertions[i].Score, want.Score) {
					t.Fatalf("component %d expected %+v; Hypercall %+v", i, want, got.Assertions[i])
				}
			}
		})
	}
}

func cellKey(candidate string, vars map[string]any, repetition int) string {
	b, _ := json.Marshal([]any{candidate, vars, repetition})
	return string(b)
}

func TestEvaluationMatrixRepetitionAndAggregationConformance(t *testing.T) {
	data, err := os.ReadFile("../../test/fixtures/evaluation/expected.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Matrices []fixtureMatrix `json:"matrices"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Matrices) == 0 {
		t.Fatal("missing evaluation matrix fixtures")
	}
	for _, sample := range fixture.Matrices {
		t.Run(sample.ID, func(t *testing.T) {
			plan, err := eval.Compile(fixtureConfig(sample.Assertions, sample.Vars, nil, sample.Candidates, sample.Repetitions))
			if sample.Reject {
				if err == nil {
					t.Fatal("unsupported evaluation matrix admitted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			expected := map[string]fixtureCell{}
			for _, cell := range sample.Expected.Cells {
				expected[cellKey(cell.Candidate, cell.Vars, cell.Repetition)] = cell
			}
			if len(plan.Cells) != len(expected) {
				t.Fatalf("expected cells=%d Hypercall=%d", len(expected), len(plan.Cells))
			}
			observations := []eval.Observation{}
			for _, cell := range plan.Cells {
				want, ok := expected[cellKey(cell.CandidateID, cell.Vars, cell.Repetition)]
				if !ok {
					t.Fatalf("unexpected matrix cell %+v", cell)
				}
				state := "complete"
				if want.Error {
					state = "execution_error"
				}
				observations = append(observations, eval.Observation{CellID: cell.ID, Output: want.Output, State: state, Source: "synthetic", TraceComplete: true})
			}
			report, err := eval.Evaluate(context.Background(), plan, observations, nil)
			if err != nil {
				t.Fatal(err)
			}
			for i, cell := range plan.Cells {
				want := expected[cellKey(cell.CandidateID, cell.Vars, cell.Repetition)]
				got := report.Cells[i]
				if got.Pass != want.Pass || !sameScore(got.Score, want.Score) {
					t.Fatalf("cell %s expected=%+v Hypercall=%+v", cell.ID, want, got)
				}
			}
			if report.Passed != sample.Expected.Passed || !sameScore(report.MeanScore, sample.Expected.MeanScore) {
				t.Fatalf("expected aggregate %+v Hypercall %+v", sample.Expected, report)
			}
			if report.Passed != report.Expected && report.CI.Pass {
				t.Fatal("failed/missing expected output passed CI")
			}
		})
	}
}

func sameScore(got, want float64) bool {
	return !math.IsNaN(got) && !math.IsInf(got, 0) && math.Abs(got-want) <= 1e-12
}
