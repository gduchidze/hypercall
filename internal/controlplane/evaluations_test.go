package controlplane_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	cp "github.com/gduchidze/hypercall/internal/controlplane"
	"github.com/gduchidze/hypercall/internal/eval"
)

const evalYAML = `apiVersion: hypercall.io/eval/v1
scope: whole_task
workload_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
candidates: [{id: fixture, model_digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb}]
cases: [{id: greeting, assert: [{type: equals, value: hello}]}]
repetitions: 1
`

func TestEvaluationAdmissionIsDurableScopedAndIdempotent(t *testing.T) {
	st, svc, scope := fixture(t)
	ctx := context.Background()
	plan, err := eval.Compile([]byte(evalYAML))
	if err != nil {
		t.Fatal(err)
	}
	output := "hello"
	obs := []eval.Observation{{CellID: plan.Cells[0].ID, Output: &output, State: "complete", Source: "synthetic", TraceComplete: true}}
	admitted, err := svc.SubmitEvaluation(ctx, scope, evalYAML, obs, "evaluate")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := svc.SubmitEvaluation(ctx, scope, evalYAML, obs, "evaluate")
	if err != nil || replay.ID != admitted.ID {
		t.Fatalf("replay changed evaluation: %+v %v", replay, err)
	}
	if _, err = svc.GetEvaluation(ctx, cp.Scope{TenantID: "other", ProjectID: scope.ProjectID}, admitted.ID); cp.Code(err) != "not_found" {
		t.Fatalf("scope escaped: %v", err)
	}
	if _, err = svc.GetReport(ctx, scope, admitted.ID); cp.Code(err) != "failed_precondition" {
		t.Fatalf("pending report appears complete: %v", err)
	}
	if err = svc.WorkEvaluation(ctx, "test-worker", time.Second, nil); err != nil {
		t.Fatal(err)
	}
	report, err := svc.GetReport(ctx, scope, admitted.ID)
	if err != nil || !report.CI.Pass || report.Passed != 1 {
		t.Fatalf("unexpected report %+v %v", report, err)
	}
	st.Close()
	reopened, err := cp.Open(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	retained, err := cp.NewService(reopened, cp.UnavailableBackend{}).GetReport(ctx, scope, admitted.ID)
	if err != nil || retained.PlanDigest != report.PlanDigest {
		t.Fatalf("restart lost report: %+v %v", retained, err)
	}
	output = "changed"
	_, err = cp.NewService(reopened, cp.UnavailableBackend{}).SubmitEvaluation(ctx, scope, evalYAML, obs, "evaluate")
	if cp.Code(err) != "already_exists" {
		t.Fatalf("changed retry accepted: %v", err)
	}
}

func TestBranchEvaluationResolvesCommittedEvidenceAndRejectsForgedMeasurements(t *testing.T) {
	_, svc, scope, children := forkedAgents(t)
	a := children[0]
	ctx := context.Background()
	finishAgentSuffix(t, svc, a)
	var task cp.TaskSpec
	_ = json.Unmarshal(a.Run.Task.Spec, &task)
	cfg := eval.Config{APIVersion: "hypercall.io/eval/v1", Scope: "continuation", WorkloadDigest: task.DatasetDigest, Candidates: []eval.Candidate{{ID: "candidate", ModelDigest: a.SelectedModel.Metadata.Digest}}, Cases: []eval.Case{{ID: "case", Assertions: []eval.Assertion{{Type: "contains", Value: "output"}}}}, Repetitions: 1}
	raw, _ := json.Marshal(cfg)
	p, err := eval.Compile(raw)
	if err != nil {
		t.Fatal(err)
	}
	obs := []eval.Observation{{CellID: p.Cells[0].ID, Source: "branch", BranchID: a.BranchID, CallID: "infer"}}
	admitted, err := svc.SubmitEvaluation(ctx, scope, string(raw), obs, "branch-eval")
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.WorkEvaluation(ctx, "worker", time.Second, nil); err != nil {
		t.Fatal(err)
	}
	r, err := svc.GetReport(ctx, scope, admitted.ID)
	if err != nil || !r.CI.Pass || !r.Synthetic || r.TotalCostMicroUSD != nil {
		t.Fatalf("branch evidence invalid %+v %v", r, err)
	}
	obs[0].Measurements = &eval.Measurements{Cost: &eval.Cost{MicroUSD: 0, Kind: "measured", PriceDigest: "sha256:" + strings.Repeat("d", 64), RecordedAt: "2026-11-04T00:00:00Z"}}
	if _, err = svc.SubmitEvaluation(ctx, scope, string(raw), obs, "forged"); cp.Code(err) != "invalid_argument" {
		t.Fatalf("client invented branch cost: %v", err)
	}
	obs[0].Measurements = nil
	if _, err = svc.SubmitEvaluation(ctx, cp.Scope{TenantID: "other", ProjectID: scope.ProjectID}, string(raw), obs, "other"); cp.Code(err) != "not_found" {
		t.Fatalf("branch tenant escaped: %v", err)
	}
}

func TestRegressionGateUsesImmutableCompatibleScopedBaseline(t *testing.T) {
	_, svc, scope := fixture(t)
	ctx := context.Background()
	p, _ := eval.Compile([]byte(evalYAML))
	good, bad := "hello", "wrong"
	obs := []eval.Observation{{CellID: p.Cells[0].ID, State: "complete", Output: &good, Source: "synthetic", TraceComplete: true}}
	base, err := svc.SubmitEvaluation(ctx, scope, evalYAML, obs, "baseline")
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.WorkEvaluation(ctx, "worker", time.Second, nil); err != nil {
		t.Fatal(err)
	}
	config := evalYAML + "baseline_report_id: " + base.ID + "\npolicy:\n  minimum_pass_rate: 0\n  max_score_drop: 0.1\n"
	obs[0].Output = &bad
	next, err := svc.SubmitEvaluation(ctx, scope, config, obs, "regression")
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.WorkEvaluation(ctx, "worker", time.Second, nil); err != nil {
		t.Fatal(err)
	}
	r, err := svc.GetReport(ctx, scope, next.ID)
	if err != nil || r.CI.Pass || !strings.Contains(strings.Join(r.CI.Reasons, ","), "quality_regression") {
		t.Fatalf("regression passed: %+v %v", r, err)
	}
	if _, err = svc.SubmitEvaluation(ctx, cp.Scope{TenantID: "other", ProjectID: scope.ProjectID}, config, obs, "other"); cp.Code(err) != "not_found" {
		t.Fatalf("baseline scope escaped: %v", err)
	}
	if _, err = svc.SubmitEvaluation(ctx, scope, strings.Replace(config, "value: hello", "value: different", 1), obs, "incompatible"); cp.Code(err) != "invalid_argument" {
		t.Fatalf("incompatible baseline accepted: %v", err)
	}
}

func TestBranchEvidenceCannotInflateRepetitionsAndRetryRetainsFirstCapture(t *testing.T) {
	_, svc, scope, children := forkedAgents(t)
	a := children[0]
	ctx := context.Background()
	payload := []byte("same-question")
	_, err := svc.ProcessAgentRequest(ctx, a, cp.AgentRequest{ID: "infer", Kind: "inference", Step: 2, Position: 1, Name: "inference", Payload: payload, PayloadDigest: pinBytes(payload), SessionHash: a.HarnessSession.SessionHash})
	if err != nil {
		t.Fatal(err)
	}
	var task cp.TaskSpec
	_ = json.Unmarshal(a.Run.Task.Spec, &task)
	cfg := eval.Config{APIVersion: "hypercall.io/eval/v1", Scope: "continuation", WorkloadDigest: task.DatasetDigest, Candidates: []eval.Candidate{{ID: "candidate", ModelDigest: a.SelectedModel.Metadata.Digest}}, Cases: []eval.Case{{ID: "case", Assertions: []eval.Assertion{{Type: "contains", Value: "output"}}}}, Repetitions: 1}
	raw, _ := json.Marshal(cfg)
	p, _ := eval.Compile(raw)
	obs := []eval.Observation{{CellID: p.Cells[0].ID, Source: "branch", BranchID: a.BranchID, CallID: "infer"}}
	first, err := svc.SubmitEvaluation(ctx, scope, string(raw), obs, "capture")
	if err != nil {
		t.Fatal(err)
	}
	finishAgentSuffix(t, svc, a)
	retry, err := svc.SubmitEvaluation(ctx, scope, string(raw), obs, "capture")
	if err != nil || retry.ID != first.ID {
		t.Fatalf("reference retry re-captured mutable trace status %+v %v", retry, err)
	}
	if err = svc.WorkEvaluation(ctx, "worker", time.Second, nil); err != nil {
		t.Fatal(err)
	}
	report, err := svc.GetReport(ctx, scope, first.ID)
	if err != nil || report.CI.Pass {
		t.Fatalf("first incomplete capture was overwritten %+v %v", report, err)
	}
	cfg.Repetitions = 2
	raw, _ = json.Marshal(cfg)
	p, _ = eval.Compile(raw)
	obs = append(obs, eval.Observation{CellID: p.Cells[1].ID, Source: "branch", BranchID: a.BranchID, CallID: "infer"})
	if _, err = svc.SubmitEvaluation(ctx, scope, string(raw), obs, "duplicate"); cp.Code(err) != "invalid_argument" {
		t.Fatalf("one branch inflated sample count: %v", err)
	}
	cfg.Repetitions = 1
	cfg.SharedPrefix = &eval.SharedPrefix{ID: "other-checkpoint"}
	raw, _ = json.Marshal(cfg)
	if _, err = svc.SubmitEvaluation(ctx, scope, string(raw), obs[:1], "wrong-prefix"); cp.Code(err) != "invalid_argument" {
		t.Fatalf("unrelated prefix admitted: %v", err)
	}
}

type canceledGrader struct{ started chan struct{} }

func (g canceledGrader) Score(ctx context.Context, _ eval.Assertion, _ string, _ map[string]any) (eval.AssertionResult, error) {
	close(g.started)
	<-ctx.Done()
	return eval.AssertionResult{}, ctx.Err()
}

type passingGrader struct{}

func (passingGrader) Score(context.Context, eval.Assertion, string, map[string]any) (eval.AssertionResult, error) {
	return eval.AssertionResult{Pass: true, Score: 1, Cost: &eval.GradingCost{Amount: .00001, Currency: "USD", Kind: "synthetic", Source: "fixture"}}, nil
}
func TestEvaluationWorkerRecoversExpiredWorkWithoutPublishingCanceledResult(t *testing.T) {
	_, svc, scope := fixture(t)
	ctx := context.Background()
	config := strings.Replace(evalYAML, "type: equals, value: hello", "type: python, scorer_digest: sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", 1)
	p, err := eval.Compile([]byte(config))
	if err != nil {
		t.Fatal(err)
	}
	output := "hello"
	latency := int64(10)
	admitted, err := svc.SubmitEvaluation(ctx, scope, config, []eval.Observation{{CellID: p.Cells[0].ID, Output: &output, State: "complete", Source: "synthetic", TraceComplete: true, Measurements: &eval.Measurements{LatencyMS: &latency, Cost: &eval.Cost{MicroUSD: 10, Kind: "synthetic", PriceDigest: pinBytes([]byte("price")), RecordedAt: "2026-11-04T00:00:00Z"}}}}, "crash")
	if err != nil {
		t.Fatal(err)
	}
	work, cancel := context.WithCancel(ctx)
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- svc.WorkEvaluation(work, "old", 30*time.Millisecond, canceledGrader{started}) }()
	<-started
	cancel()
	if err = <-done; err == nil {
		t.Fatal("canceled owner published a report")
	}
	if _, err = svc.GetReport(ctx, scope, admitted.ID); cp.Code(err) != "failed_precondition" {
		t.Fatalf("canceled score visible: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if err = svc.WorkEvaluation(ctx, "replacement", time.Second, passingGrader{}); err != nil {
		t.Fatal(err)
	}
	r, err := svc.GetReport(ctx, scope, admitted.ID)
	if err != nil || !r.CI.Pass || r.Candidates[0].CostMicroUSD != nil || len(r.Pareto) != 0 {
		t.Fatalf("recovery failed: %+v %v", r, err)
	}
	v, err := svc.GetEvaluation(ctx, scope, admitted.ID)
	if err != nil || v.Attempts != 2 {
		t.Fatalf("attempt accounting lost: %+v %v", v, err)
	}
}
