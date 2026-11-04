package eval_test

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"

	pb "github.com/gduchidze/hypercall/gen/go/hypercall/v1"
	"github.com/gduchidze/hypercall/internal/eval"
	"google.golang.org/protobuf/proto"
)

func TestContinuationAccountsSharedPrefixOnceAndKeepsMissingCostsUnknown(t *testing.T) {
	cfg := eval.Config{APIVersion: "hypercall.io/eval/v1", Scope: "continuation", WorkloadDigest: "sha256:" + strings.Repeat("a", 64), Candidates: []eval.Candidate{{ID: "cheap", ModelDigest: "sha256:" + strings.Repeat("b", 64)}, {ID: "expensive", ModelDigest: "sha256:" + strings.Repeat("c", 64)}}, Cases: []eval.Case{{ID: "case", Assertions: []eval.Assertion{{Type: "equals", Value: "ok"}}}}, Repetitions: 2, Policy: eval.Policy{MinimumSamples: 2, MaxCostMicroUSD: int64ptr(200)}, SharedPrefix: &eval.SharedPrefix{ID: "checkpoint-1", Cost: &eval.Cost{MicroUSD: 100, Kind: "synthetic", PriceDigest: "sha256:" + strings.Repeat("d", 64), RecordedAt: "2026-11-04T00:00:00Z"}}}
	data, _ := json.Marshal(cfg)
	p, err := eval.Compile(data)
	if err != nil {
		t.Fatal(err)
	}
	output := "ok"
	obs := []eval.Observation{}
	for _, cell := range p.Cells {
		cost := int64(10)
		latency := int64(10)
		if cell.CandidateID == "expensive" {
			cost = 30
			latency = 20
		}
		obs = append(obs, eval.Observation{CellID: cell.ID, Output: &output, State: "complete", Source: "synthetic", TraceComplete: true, Measurements: &eval.Measurements{LatencyMS: &latency, Cost: &eval.Cost{MicroUSD: cost, Kind: "synthetic", PriceDigest: "sha256:" + strings.Repeat("d", 64), RecordedAt: "2026-11-04T00:00:00Z"}}})
	}
	r, err := eval.Evaluate(context.Background(), p, obs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.TotalCostMicroUSD == nil || *r.TotalCostMicroUSD != 180 || !r.CI.Pass || len(r.Pareto) != 1 || r.Pareto[0] != "cheap" {
		t.Fatalf("unexpected accounting %+v", r)
	}
	obs[0].Measurements.Cost = nil
	r, err = eval.Evaluate(context.Background(), p, obs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.TotalCostMicroUSD != nil || r.CI.Pass || !strings.Contains(strings.Join(r.CI.Reasons, ","), "measurement_missing") {
		t.Fatalf("unknown cost became free: %+v", r)
	}
}
func int64ptr(v int64) *int64 { return &v }

func TestLatencyAndCostAssertionsRequireRecordedMeasurements(t *testing.T) {
	cfg := eval.Config{APIVersion: "hypercall.io/eval/v1", Scope: "whole_task", WorkloadDigest: "sha256:" + strings.Repeat("a", 64), Candidates: []eval.Candidate{{ID: "m", ModelDigest: "sha256:" + strings.Repeat("b", 64)}}, Cases: []eval.Case{{ID: "case", Threshold: float(0), Assertions: []eval.Assertion{{Type: "latency", Threshold: float(20)}, {Type: "cost", Threshold: float(0.000020)}}}}, Repetitions: 1}
	data, _ := json.Marshal(cfg)
	p, err := eval.Compile(data)
	if err != nil {
		t.Fatal(err)
	}
	output := "ok"
	o := eval.Observation{CellID: p.Cells[0].ID, Output: &output, State: "complete", Source: "synthetic", TraceComplete: true, Measurements: &eval.Measurements{LatencyMS: int64ptr(20), Cost: &eval.Cost{MicroUSD: 20, Kind: "synthetic", PriceDigest: "sha256:" + strings.Repeat("d", 64), RecordedAt: "2026-11-04T00:00:00Z"}}}
	r, err := eval.Evaluate(context.Background(), p, []eval.Observation{o}, nil)
	if err != nil || !r.CI.Pass {
		t.Fatalf("boundary failed %+v %v", r, err)
	}
	o.Measurements = nil
	r, err = eval.Evaluate(context.Background(), p, []eval.Observation{o}, nil)
	if err != nil || r.CI.Pass || r.Cells[0].State != "evaluation_error" {
		t.Fatalf("unknown measurement passed %+v %v", r, err)
	}
}

func TestMinimumPassRateUsesAllPlannedSamples(t *testing.T) {
	cfg := eval.Config{APIVersion: "hypercall.io/eval/v1", Scope: "whole_task", WorkloadDigest: "sha256:" + strings.Repeat("a", 64), Candidates: []eval.Candidate{{ID: "m", ModelDigest: "sha256:" + strings.Repeat("b", 64)}}, Cases: []eval.Case{{ID: "case", Assertions: []eval.Assertion{{Type: "equals", Value: "ok"}}}}, Repetitions: 2, Policy: eval.Policy{MinimumPassRate: float(.5)}}
	data, _ := json.Marshal(cfg)
	p, err := eval.Compile(data)
	if err != nil {
		t.Fatal(err)
	}
	good, bad := "ok", "wrong"
	r, err := eval.Evaluate(context.Background(), p, []eval.Observation{{CellID: p.Cells[0].ID, State: "complete", Output: &good, Source: "synthetic", TraceComplete: true}, {CellID: p.Cells[1].ID, State: "complete", Output: &bad, Source: "synthetic", TraceComplete: true}}, nil)
	if err != nil || !r.CI.Pass || r.PassRate != .5 {
		t.Fatalf("policy ignored %+v %v", r, err)
	}
}

type pricedScorer struct{ cost *eval.GradingCost }

func (s pricedScorer) Score(context.Context, eval.Assertion, string, map[string]any) (eval.AssertionResult, error) {
	return eval.AssertionResult{Pass: true, Score: 1, Cost: s.cost}, nil
}
func TestBudgetIncludesSeparatelyReportedScoringCost(t *testing.T) {
	cfg := eval.Config{APIVersion: "hypercall.io/eval/v1", Scope: "whole_task", WorkloadDigest: "sha256:" + strings.Repeat("a", 64), Candidates: []eval.Candidate{{ID: "m", ModelDigest: "sha256:" + strings.Repeat("b", 64)}}, Cases: []eval.Case{{ID: "case", Assertions: []eval.Assertion{{Type: "python", ScorerDigest: "sha256:" + strings.Repeat("c", 64)}}}}, Repetitions: 1, Policy: eval.Policy{MaxCostMicroUSD: int64ptr(20)}}
	data, _ := json.Marshal(cfg)
	p, err := eval.Compile(data)
	if err != nil {
		t.Fatal(err)
	}
	output := "ok"
	obs := []eval.Observation{{CellID: p.Cells[0].ID, Output: &output, State: "complete", Source: "synthetic", TraceComplete: true, Measurements: &eval.Measurements{Cost: &eval.Cost{MicroUSD: 10, Kind: "synthetic", PriceDigest: "sha256:" + strings.Repeat("d", 64), RecordedAt: "2026-11-04T00:00:00Z"}}}}
	r, err := eval.Evaluate(context.Background(), p, obs, pricedScorer{&eval.GradingCost{Amount: .00002, Currency: "USD", Kind: "synthetic", Source: "fixture"}})
	if err != nil || r.CI.Pass || r.ScoringCostMicroUSD == nil || *r.ScoringCostMicroUSD != 20 || r.TotalCostMicroUSD == nil || *r.TotalCostMicroUSD != 30 || r.Candidates[0].CostMicroUSD == nil || *r.Candidates[0].CostMicroUSD != 30 {
		t.Fatalf("scoring cost omitted %+v %v", r, err)
	}
	r, err = eval.Evaluate(context.Background(), p, obs, pricedScorer{})
	if err != nil || r.TotalCostMicroUSD != nil || r.CI.Pass {
		t.Fatalf("unknown scoring became free %+v %v", r, err)
	}
}

func TestUnscoredEvidenceCannotBecomeAParetoWinner(t *testing.T) {
	cfg := eval.Config{APIVersion: "hypercall.io/eval/v1", Scope: "whole_task", WorkloadDigest: "sha256:" + strings.Repeat("a", 64), Candidates: []eval.Candidate{{ID: "m", ModelDigest: "sha256:" + strings.Repeat("b", 64)}}, Cases: []eval.Case{{ID: "case", Assertions: []eval.Assertion{{Type: "equals", Value: "ok"}}}}, Repetitions: 1, Policy: eval.Policy{MinimumPassRate: float(0)}}
	data, _ := json.Marshal(cfg)
	p, err := eval.Compile(data)
	if err != nil {
		t.Fatal(err)
	}
	output := "ok"
	o := eval.Observation{CellID: p.Cells[0].ID, Source: "synthetic", State: "complete", Output: &output, TraceComplete: false, Measurements: &eval.Measurements{LatencyMS: int64ptr(1), Cost: &eval.Cost{MicroUSD: 1, Kind: "synthetic", PriceDigest: "sha256:" + strings.Repeat("c", 64), RecordedAt: "2026-11-04T00:00:00Z"}}}
	r, err := eval.Evaluate(context.Background(), p, []eval.Observation{o}, nil)
	if err != nil || r.CI.Pass || r.Candidates[0].Completed != 0 || r.ScoreSamples != 0 || len(r.Pareto) != 0 {
		t.Fatalf("incomplete winner %+v %v", r, err)
	}
	r, err = eval.Evaluate(context.Background(), p, nil, nil)
	if err != nil || !r.Synthetic {
		t.Fatalf("missing synthetic evidence fabricated a real measurement %+v %v", r, err)
	}
}

type verboseScorer struct{}

func (verboseScorer) Score(context.Context, eval.Assertion, string, map[string]any) (eval.AssertionResult, error) {
	return eval.AssertionResult{Pass: true, Score: 1, Reason: strings.Repeat("\x01", 4096), Cost: &eval.GradingCost{Amount: 0, Currency: "USD", Kind: "synthetic", Source: strings.Repeat("\x01", 1024)}}, nil
}
func TestAdmittedReportsRemainRetrievableAndRejectNonfiniteGraderCosts(t *testing.T) {
	cfg := eval.Config{APIVersion: "hypercall.io/eval/v1", Scope: "whole_task", WorkloadDigest: "sha256:" + strings.Repeat("a", 64), Candidates: []eval.Candidate{{ID: "m", ModelDigest: "sha256:" + strings.Repeat("b", 64)}}, Cases: []eval.Case{{ID: "case", Assertions: []eval.Assertion{{Type: "python", ScorerDigest: "sha256:" + strings.Repeat("c", 64)}}}}, Repetitions: 1000}
	data, _ := json.Marshal(cfg)
	p, err := eval.Compile(data)
	if err != nil {
		t.Fatal(err)
	}
	output := "ok"
	obs := make([]eval.Observation, len(p.Cells))
	for i, cell := range p.Cells {
		obs[i] = eval.Observation{CellID: cell.ID, Output: &output, State: "complete", Source: "synthetic", TraceComplete: true}
	}
	r, err := eval.Evaluate(context.Background(), p, obs, verboseScorer{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(r)
	if err != nil || len(encoded)+len(eval.Markdown(r)) > 4<<20 {
		t.Fatalf("admitted report exceeds public response limit: json=%d markdown=%d err=%v", len(encoded), len(eval.Markdown(r)), err)
	}
	if proto.Size(&pb.EvaluationReport{ReportJson: encoded, Markdown: eval.Markdown(r), Reasons: r.CI.Reasons}) > 4<<20 {
		t.Fatal("protobuf report exceeds public envelope")
	}
	p.Baseline = r
	planJSON, err := json.Marshal(p)
	if err != nil || proto.Size(&pb.Evaluation{PlanJson: planJSON}) > 4<<20 {
		t.Fatalf("evaluation inspection exceeds public envelope: %d %v", len(planJSON), err)
	}
	p.Baseline = nil
	cfg.Repetitions = 1001
	data, _ = json.Marshal(cfg)
	if _, err = eval.Compile(data); err == nil {
		t.Fatal("matrix exceeds supported report envelope")
	}
	r, err = eval.Evaluate(context.Background(), p, obs, pricedScorer{&eval.GradingCost{Amount: math.NaN(), Currency: "USD", Kind: "synthetic", Source: "fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = json.Marshal(r); err != nil || r.CI.Pass || r.Cells[0].State != "evaluation_error" || r.Cells[0].Assertions[0].Cost != nil {
		t.Fatalf("nonfinite scorer cost escaped: err=%v first=%+v", err, r.Cells[0])
	}
}
