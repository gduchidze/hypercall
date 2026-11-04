package api

import (
	"context"
	"encoding/json"

	pb "github.com/gduchidze/hypercall/gen/go/hypercall/v1"
	cp "github.com/gduchidze/hypercall/internal/controlplane"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

func (s *Server) Fork(ctx context.Context, req *pb.ForkRequest) (*pb.ForkResult, error) {
	sc, e := scope(ctx, req.ProjectId)
	if e != nil {
		return nil, e
	}
	models := make([]cp.Ref, 0, len(req.Models))
	for _, ref := range req.Models {
		if ref == nil {
			return nil, status.Error(codes.InvalidArgument, "candidate reference required")
		}
		models = append(models, cp.Ref{ID: ref.Id, Digest: ref.Digest})
	}
	fork, e := s.service.Fork(ctx, sc, req.CheckpointId, models, req.IdempotencyKey)
	if e != nil {
		return nil, canonical(e)
	}
	operation, e := resource(fork.Operation)
	if e != nil {
		return nil, canonical(e)
	}
	out := &pb.ForkResult{Operation: operation}
	for _, child := range fork.Children {
		v, e := resource(child)
		if e != nil {
			return nil, canonical(e)
		}
		out.Children = append(out.Children, v)
	}
	return out, nil
}
func traceLimit(after int64, limit int32) (int, error) {
	if after < 0 || limit < 0 || limit > 1000 {
		return 0, status.Error(codes.InvalidArgument, "nonnegative cursor and page limit <=1000 required")
	}
	if limit == 0 {
		return 100, nil
	}
	return int(limit), nil
}
func (s *Server) Calls(ctx context.Context, req *pb.CallsRequest) (*pb.CallsResponse, error) {
	sc, e := scope(ctx, req.ProjectId)
	if e != nil {
		return nil, e
	}
	limit, e := traceLimit(req.AfterSequence, req.Limit)
	if e != nil {
		return nil, e
	}
	calls, e := s.service.Calls(ctx, sc, req.BranchId, req.AfterSequence, limit)
	if e != nil {
		return nil, canonical(e)
	}
	out := &pb.CallsResponse{NextSequence: req.AfterSequence}
	for _, call := range calls {
		raw, e := json.Marshal(call)
		if e != nil {
			return nil, canonical(e)
		}
		v := &pb.AgentCall{}
		if e = protojson.Unmarshal(raw, v); e != nil {
			return nil, canonical(e)
		}
		out.Calls = append(out.Calls, v)
		out.NextSequence = call.RegisteredSequence
	}
	return out, nil
}
func (s *Server) Steps(ctx context.Context, req *pb.StepsRequest) (*pb.StepsResponse, error) {
	sc, e := scope(ctx, req.ProjectId)
	if e != nil {
		return nil, e
	}
	limit, e := traceLimit(req.AfterStep, req.Limit)
	if e != nil {
		return nil, e
	}
	steps, e := s.service.Steps(ctx, sc, req.BranchId, req.AfterStep, limit)
	if e != nil {
		return nil, canonical(e)
	}
	out := &pb.StepsResponse{NextStep: req.AfterStep}
	for _, step := range steps {
		out.Steps = append(out.Steps, &pb.AgentStep{BranchId: step.BranchID, Step: step.Step, StateDigest: step.StateDigest, EventSequence: step.EventSequence, SessionHash: step.SessionHash, Complete: step.Complete})
		out.NextStep = step.Step
	}
	return out, nil
}

func (s *Server) Compare(ctx context.Context, req *pb.CompareRequest) (*pb.Divergence, error) {
	sc, e := scope(ctx, req.ProjectId)
	if e != nil {
		return nil, e
	}
	result, e := s.service.Compare(ctx, sc, req.LeftBranch, req.RightBranch, req.Comparator)
	if e != nil {
		return nil, canonical(e)
	}
	return &pb.Divergence{Comparator: result.Comparator, State: result.State, Step: result.Step, Position: result.Position, LeftDigest: result.LeftDigest, RightDigest: result.RightDigest, Reason: result.Reason}, nil
}
