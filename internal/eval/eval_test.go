package eval_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gduchidze/hypercall/internal/eval"
)

func TestEvaluationExpandsCasesAndScoresEachRecordedOutput(t *testing.T) {
	plan, err := eval.Compile([]byte(`apiVersion: hypercall.io/eval/v1
scope: whole_task
workload_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
candidates:
  - id: model-a
    model_digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  - id: model-b
    model_digest: sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
cases:
  - id: greeting
    vars:
      name: [Alice, Bob]
    assert:
      - type: equals
        value: "Hello {{name}}"
        weight: 3
      - type: contains
        value: Hello
        weight: 1
repetitions: 1
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Cells) != 4 {
		t.Fatalf("expected four matrix cells, got %d", len(plan.Cells))
	}
	observations := make([]eval.Observation, 0, len(plan.Cells))
	for _, cell := range plan.Cells {
		output := "Hello " + cell.Vars["name"].(string)
		if cell.CandidateID == "model-b" {
			output = "Hello Carol"
		}
		observations = append(observations, eval.Observation{CellID: cell.ID, Output: &output, State: "complete", Source: "synthetic", TraceComplete: true})
	}
	report, err := eval.Evaluate(context.Background(), plan, observations, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.Expected != 4 || report.Completed != 4 || report.Passed != 2 || report.MeanScore != 0.625 || report.CI.Pass {
		t.Fatalf("unexpected report: %+v", report)
	}
	for _, cell := range report.Cells {
		want := 1.0
		if cell.CandidateID == "model-b" {
			want = 0.25
		}
		if cell.Score != want {
			t.Fatalf("candidate %s score %g, want %g", cell.CandidateID, cell.Score, want)
		}
	}
}

func TestAssertionsMatchRecordedEdgeCases(t *testing.T) {
	for _, tc := range []struct {
		name, kind, output, value string
		weight, threshold         *float64
		score                     float64
		pass                      bool
	}{
		{"object equality", "equals", `{"b":2,"a":1}`, `{"a":1,"b":2}`, nil, nil, 1, true},
		{"invalid JSON negation", "not-equals", `broken`, `{"a":1}`, nil, nil, 1, true},
		{"unicode literal", "contains", "γειά σου 世界", `"世界"`, nil, nil, 1, true},
		{"negated contains", "not-contains", "safe", `"unsafe"`, nil, nil, 1, true},
		{"valid JSON null", "is-json", "null", `null`, nil, nil, 1, true},
		{"invalid JSON", "is-json", "{} trailing", `null`, nil, nil, 0, false},
		{"metric only", "equals", "wrong", `"expected"`, float(0), nil, 0, true},
		{"threshold zero", "equals", "wrong", `"expected"`, nil, float(0), 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var value any
			if err := json.Unmarshal([]byte(tc.value), &value); err != nil {
				t.Fatal(err)
			}
			cfg := eval.Config{APIVersion: "hypercall.io/eval/v1", Scope: "whole_task", WorkloadDigest: "sha256:" + strings.Repeat("a", 64), Candidates: []eval.Candidate{{ID: "m", ModelDigest: "sha256:" + strings.Repeat("b", 64)}}, Cases: []eval.Case{{ID: "case", Assertions: []eval.Assertion{{Type: tc.kind, Value: value, Weight: tc.weight}}, Threshold: tc.threshold}}, Repetitions: 1}
			data, _ := json.Marshal(cfg)
			p, err := eval.Compile(data)
			if err != nil {
				t.Fatal(err)
			}
			r, err := eval.Evaluate(context.Background(), p, []eval.Observation{{CellID: p.Cells[0].ID, State: "complete", Output: &tc.output, Source: "synthetic", TraceComplete: true}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if r.Cells[0].Pass != tc.pass || r.Cells[0].Score != tc.score {
				t.Fatalf("got %+v", r.Cells[0])
			}
		})
	}
}
func float(v float64) *float64 { return &v }

type unavailableScorer struct{}

func (unavailableScorer) Score(context.Context, eval.Assertion, string, map[string]any) (eval.AssertionResult, error) {
	return eval.AssertionResult{}, errors.New("worker deadline")
}
func TestScorerFailureNeverPassesZeroThreshold(t *testing.T) {
	data := []byte(`apiVersion: hypercall.io/eval/v1
scope: whole_task
workload_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
candidates: [{id: m, model_digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb}]
cases:
- id: scorer
  threshold: 0
  assert:
  - type: python
    scorer_digest: sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
repetitions: 1
`)
	p, err := eval.Compile(data)
	if err != nil {
		t.Fatal(err)
	}
	output := "anything"
	r, err := eval.Evaluate(context.Background(), p, []eval.Observation{{CellID: p.Cells[0].ID, Output: &output, State: "complete", Source: "synthetic", TraceComplete: true}}, unavailableScorer{})
	if err != nil {
		t.Fatal(err)
	}
	if r.CI.Pass || r.Cells[0].State != "evaluation_error" || r.Cells[0].Pass {
		t.Fatalf("failed scorer passed %+v", r)
	}
}

func TestJSONSchemaRejectsInvalidRequiredAndNumericValues(t *testing.T) {
	data := []byte(`apiVersion: hypercall.io/eval/v1
scope: whole_task
workload_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
candidates: [{id: m, model_digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb}]
cases:
- id: schema
  assert:
  - type: is-json
    value:
      type: object
      required: [score]
      additionalProperties: false
      properties:
        score: {type: number, minimum: 0, maximum: 1}
repetitions: 1
`)
	p, err := eval.Compile(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		output string
		pass   bool
	}{{`{"score":0.5}`, true}, {`{"score":2}`, false}, {`{}`, false}, {`{"score":0,"extra":1}`, false}} {
		r, err := eval.Evaluate(context.Background(), p, []eval.Observation{{CellID: p.Cells[0].ID, Output: &tc.output, State: "complete", Source: "synthetic", TraceComplete: true}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if r.CI.Pass != tc.pass {
			t.Fatalf("schema got %+v for %s", r, tc.output)
		}
	}
	if _, err := eval.Compile([]byte(strings.Replace(string(data), "type: number, minimum: 0, maximum: 1", "$ref: https://remote.invalid/schema", 1))); err == nil {
		t.Fatal("remote schema admitted")
	}
}

func TestIncompleteEvidenceCannotPassAZeroThreshold(t *testing.T) {
	data := []byte(`{"apiVersion":"hypercall.io/eval/v1","scope":"whole_task","workload_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","candidates":[{"id":"m","model_digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}],"cases":[{"id":"case","threshold":0,"assert":[{"type":"equals","value":"ok"}]}],"repetitions":1}`)
	p, err := eval.Compile(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"missing", "execution_error", "evaluation_error", "complete"} {
		r, err := eval.Evaluate(context.Background(), p, []eval.Observation{{CellID: p.Cells[0].ID, State: state, Source: "synthetic"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if r.CI.Pass || r.Passed != 0 || !strings.Contains(strings.Join(r.CI.Reasons, ","), "incomplete_matrix") {
			t.Fatalf("invalid success: %+v", r)
		}
	}
	if _, err := eval.Evaluate(context.Background(), p, []eval.Observation{{CellID: "unknown", Source: "synthetic"}}, nil); err == nil {
		t.Fatal("unknown evidence cell admitted")
	}
}

func TestCompileRejectsUnsupportedOrAmbiguousConfiguration(t *testing.T) {
	base := `apiVersion: hypercall.io/eval/v1
scope: whole_task
workload_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
candidates: [{id: candidate, model_digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb}]
cases: [{id: example, assert: [{type: equals, value: hello}]}]
repetitions: 1
`
	for _, data := range []string{
		base + "unsupported: true\n",
		strings.Replace(base, "type: equals", "type: javascript", 1),
		strings.Replace(base, "value: hello", "value: '{{name | upper}}'", 1),
		strings.Replace(base, "value: hello", "value: '{{missing}}'", 1),
		strings.Replace(base, "value: hello", "value: hello, weight: -1", 1),
		strings.Replace(base, ", value: hello", "", 1),
		strings.Replace(base, "repetitions: 1", "repetitions: 10001", 1),
		strings.Replace(base, "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "latest", 1),
	} {
		if _, err := eval.Compile([]byte(data)); err == nil {
			t.Fatalf("unsupported configuration admitted: %s", data)
		}
	}
}
