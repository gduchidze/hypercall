package api

import (
	"context"
	"encoding/json"

	pb "github.com/gduchidze/hypercall/gen/go/hypercall/v1"
	cp "github.com/gduchidze/hypercall/internal/controlplane"
	"github.com/gduchidze/hypercall/internal/eval"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *Server) CompileEvaluation(ctx context.Context, req *pb.CompileEvaluationRequest) (*pb.EvaluationPlan, error) {
	if _, err := scope(ctx, req.ProjectId); err != nil {
		return nil, err
	}
	p, err := eval.Compile([]byte(req.ConfigYaml))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	data, err := json.Marshal(p)
	return &pb.EvaluationPlan{Digest: p.Digest, PlanJson: data}, err
}
func (s *Server) SubmitEvaluation(ctx context.Context, req *pb.SubmitEvaluationRequest) (*pb.Evaluation, error) {
	sc, err := scope(ctx, req.ProjectId)
	if err != nil {
		return nil, err
	}
	obs := make([]eval.Observation, 0, len(req.Observations))
	for _, v := range req.Observations {
		if v == nil {
			return nil, status.Error(codes.InvalidArgument, "observation required")
		}
		o := eval.Observation{CellID: v.CellId, Source: v.Source, Output: v.Output, State: v.State, TraceComplete: v.TraceComplete, BranchID: v.BranchId, CallID: v.CallId}
		if m := v.Measurements; m != nil {
			o.Measurements = &eval.Measurements{LatencyMS: m.LatencyMs}
			if c := m.Cost; c != nil {
				o.Measurements.Cost = &eval.Cost{MicroUSD: c.MicroUsd, Kind: c.Kind, PriceDigest: c.PriceDigest, RecordedAt: c.RecordedAt}
			}
		}
		obs = append(obs, o)
	}
	v, err := s.service.SubmitEvaluation(ctx, sc, req.ConfigYaml, obs, req.IdempotencyKey)
	if err != nil {
		return nil, canonical(err)
	}
	return evaluation(v)
}
func evaluation(v cp.Evaluation) (*pb.Evaluation, error) {
	data, err := json.Marshal(v.Plan)
	if err != nil {
		return nil, err
	}
	out := &pb.Evaluation{Id: v.ID, OperationId: v.OperationID, InputDigest: v.InputDigest, State: v.State, Attempts: int32(v.Attempts), PlanJson: data, CreatedAt: timestamppb.New(v.CreatedAt)}
	if v.CompletedAt != nil {
		out.CompletedAt = timestamppb.New(*v.CompletedAt)
	}
	return out, nil
}
func (s *Server) GetEvaluation(ctx context.Context, req *pb.GetEvaluationRequest) (*pb.Evaluation, error) {
	sc, err := scope(ctx, req.ProjectId)
	if err != nil {
		return nil, err
	}
	v, err := s.service.GetEvaluation(ctx, sc, req.Id)
	if err != nil {
		return nil, canonical(err)
	}
	return evaluation(v)
}
func (s *Server) GetReport(ctx context.Context, req *pb.GetReportRequest) (*pb.EvaluationReport, error) {
	sc, err := scope(ctx, req.ProjectId)
	if err != nil {
		return nil, err
	}
	r, err := s.service.GetReport(ctx, sc, req.Id)
	if err != nil {
		return nil, canonical(err)
	}
	data, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	projection, err := s.ProjectionStatus(ctx, &pb.ProjectionStatusRequest{ProjectId: req.ProjectId})
	if err != nil {
		return nil, err
	}
	return &pb.EvaluationReport{EvaluationId: req.Id, ReportJson: data, Markdown: eval.Markdown(r), CiPass: r.CI.Pass, Reasons: r.CI.Reasons, Projection: projection}, nil
}
func (s *Server) ProjectionStatus(ctx context.Context, req *pb.ProjectionStatusRequest) (*pb.ProjectionStatusResponse, error) {
	sc, err := scope(ctx, req.ProjectId)
	if err != nil {
		return nil, err
	}
	v, err := s.service.ProjectionStatus(ctx, sc)
	if err != nil {
		return nil, canonical(err)
	}
	out := &pb.ProjectionStatusResponse{Pending: v.Pending, OldestPendingSeconds: v.OldestPendingSeconds}
	if v.LastDeliveredAt != nil {
		out.LastDeliveredAt = timestamppb.New(*v.LastDeliveredAt)
	}
	return out, nil
}
