package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"
)

func (s *Service) comparisonCalls(ctx context.Context, scope Scope, branch string, depth int) ([]AgentCall, error) {
	if depth > 32 {
		return nil, failure("failed_precondition", "branch ancestry exceeds comparator bound")
	}
	resource, e := s.GetResource(ctx, scope, "Branch", branch)
	if e != nil {
		return nil, e
	}
	var spec BranchSpec
	if e = json.Unmarshal(resource.Spec, &spec); e != nil {
		return nil, e
	}
	out := []AgentCall{}
	if spec.ParentCheckpointID != "" {
		checkpoint, e := s.GetResource(ctx, scope, "Checkpoint", spec.ParentCheckpointID)
		if e != nil {
			return nil, e
		}
		var cp CheckpointSpec
		if e = json.Unmarshal(checkpoint.Spec, &cp); e != nil {
			return nil, e
		}
		if cp.Barrier == nil || cp.BranchID != spec.ParentBranchID {
			return nil, failure("failed_precondition", "missing committed parent prefix")
		}
		prefix, e := s.comparisonCalls(ctx, scope, spec.ParentBranchID, depth+1)
		if e != nil {
			return nil, e
		}
		for _, call := range prefix {
			if call.Request.Step <= cp.Barrier.Step {
				out = append(out, call)
			}
		}
	}
	calls, e := s.Calls(ctx, scope, branch, 0, 1000)
	if e != nil {
		return nil, e
	}
	if len(calls) == 1000 {
		return nil, failure("resource_exhausted", "comparison exceeds bounded trace length")
	}
	out = append(out, calls...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Request.Step == out[j].Request.Step {
			return out[i].Request.Position < out[j].Request.Position
		}
		return out[i].Request.Step < out[j].Request.Step
	})
	return out, nil
}
func (s *Service) Compare(ctx context.Context, scope Scope, left, right, comparator string) (Divergence, error) {
	result := Divergence{Comparator: comparator}
	if e := validateScope(scope); e != nil {
		return result, e
	}
	switch comparator {
	case "output_bytes_v1", "output_normalized_v1", "tool_calls_v1", "observations_v1":
	default:
		return result, failure("invalid_argument", "unsupported declared comparator")
	}
	l, e := s.GetResource(ctx, scope, "Branch", left)
	if e != nil {
		return result, e
	}
	r, e := s.GetResource(ctx, scope, "Branch", right)
	if e != nil {
		return result, e
	}
	var ls, rs BranchSpec
	var leftStatus, rightStatus BranchStatus
	if json.Unmarshal(l.Spec, &ls) != nil || json.Unmarshal(r.Spec, &rs) != nil || json.Unmarshal(l.Status, &leftStatus) != nil || json.Unmarshal(r.Status, &rightStatus) != nil {
		return result, failure("failed_precondition", "branch trace metadata is invalid")
	}
	if leftStatus.AgentGate == "unsupported" || rightStatus.AgentGate == "unsupported" {
		result.State = "unknown"
		result.Reason = "missing cooperative instrumentation"
		return result, nil
	}
	if ls.ParentCheckpointID == "" || ls.ParentCheckpointID != rs.ParentCheckpointID || ls.RunID != rs.RunID {
		result.State = "unknown"
		result.Reason = "comparison requires continuations from the same retained checkpoint"
		return result, nil
	}
	leftCalls, e := s.comparisonCalls(ctx, scope, left, 0)
	if e != nil {
		return result, e
	}
	rightCalls, e := s.comparisonCalls(ctx, scope, right, 0)
	if e != nil {
		return result, e
	}
	for _, calls := range [][]AgentCall{leftCalls, rightCalls} {
		for _, call := range calls {
			if call.Result.State == "unknown" {
				result.State = "unknown"
				result.Reason = "unresolved tool effects; no replay or causal conclusion"
				return result, nil
			}
			if call.Result.State != "completed" {
				result.State = "incomplete"
				result.Reason = "registered action has no committed outcome"
				return result, nil
			}
		}
	}
	if !leftStatus.TraceComplete || !rightStatus.TraceComplete {
		result.State = "incomplete"
		result.Reason = "finalization or retained trace is incomplete"
		return result, nil
	}
	type item struct {
		step     int64
		position int32
		payload  []byte
	}
	selectItems := func(calls []AgentCall) []item {
		out := []item{}
		for _, call := range calls {
			payload := []byte(nil)
			switch comparator {
			case "output_bytes_v1", "output_normalized_v1":
				if call.Request.Kind != "inference" {
					continue
				}
				payload = call.Result.Payload
				if comparator == "output_normalized_v1" {
					payload = []byte(strings.TrimSpace(string(payload)))
				}
			case "tool_calls_v1":
				if call.Request.Kind != "tool" {
					continue
				}
				payload = encode(struct {
					Name    string
					Payload []byte
				}{call.Request.Name, call.Request.Payload})
			case "observations_v1":
				if call.Request.Kind != "tool" {
					continue
				}
				payload = call.Result.Payload
			}
			out = append(out, item{call.Request.Step, call.Request.Position, payload})
		}
		return out
	}
	a, b := selectItems(leftCalls), selectItems(rightCalls)
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i].step != b[i].step || a[i].position != b[i].position || !bytes.Equal(a[i].payload, b[i].payload) {
			result.State = "diverged"
			result.Step = a[i].step
			result.Position = a[i].position
			if b[i].step < a[i].step || (b[i].step == a[i].step && b[i].position < a[i].position) {
				result.Step = b[i].step
				result.Position = b[i].position
			}
			result.LeftDigest = payloadDigest(a[i].payload)
			result.RightDigest = payloadDigest(b[i].payload)
			result.Reason = "first observed difference at recorded granularity; committed parent prefix inherited, no causality claim"
			if a[i].step != b[i].step || a[i].position != b[i].position {
				result.Reason = "ordinal action alignment differs; causal step alignment is unknown"
			}
			return result, nil
		}
	}
	if len(a) != len(b) {
		result.State = "diverged"
		if len(a) > n {
			result.Step = a[n].step
			result.Position = a[n].position
			result.LeftDigest = payloadDigest(a[n].payload)
		} else {
			result.Step = b[n].step
			result.Position = b[n].position
			result.RightDigest = payloadDigest(b[n].payload)
		}
		result.Reason = "one complete trajectory has an additional observed action"
		return result, nil
	}
	result.State = "equal"
	result.Reason = "equal under declared continuation comparator, including committed inherited prefix"
	return result, nil
}
