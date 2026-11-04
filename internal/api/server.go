package api

import (
	"context"
	"encoding/json"
	"net/http"

	guest "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	pb "github.com/gduchidze/hypercall/gen/go/hypercall/v1"
	cp "github.com/gduchidze/hypercall/internal/controlplane"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Server struct {
	pb.UnimplementedControlPlaneServer
	service *cp.Service
	auth    *Authenticator
	grpc    *grpc.Server
}

func NewServer(service *cp.Service, auth *Authenticator, options ...grpc.ServerOption) *Server {
	s := &Server{service: service, auth: auth}
	options = append(options, grpc.UnaryInterceptor(auth.unary), grpc.StreamInterceptor(auth.stream), grpc.MaxRecvMsgSize(2<<20), grpc.MaxSendMsgSize(4<<20))
	s.grpc = grpc.NewServer(options...)
	pb.RegisterControlPlaneServer(s.grpc, s)
	return s
}
func (s *Server) GRPC() *grpc.Server { return s.grpc }

// HTTP registers the generated REST mapping. Scope/authentication is shared with
// gRPC. Server-streamed commands use the gateway's newline-delimited JSON format.
func (s *Server) HTTP(conn *grpc.ClientConn) http.Handler {
	mux := runtime.NewServeMux(runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.JSONPb{MarshalOptions: protojson.MarshalOptions{UseProtoNames: true}, UnmarshalOptions: protojson.UnmarshalOptions{DiscardUnknown: false}}))
	if err := pb.RegisterControlPlaneHandler(context.Background(), mux, conn); err != nil {
		panic(err)
	}
	return s.auth.HTTP(mux)
}
func canonical(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	if err == context.Canceled {
		return status.Error(codes.Canceled, "request canceled")
	}
	if err == context.DeadlineExceeded {
		return status.Error(codes.DeadlineExceeded, "request deadline exceeded")
	}
	code := map[string]codes.Code{"invalid_argument": codes.InvalidArgument, "not_found": codes.NotFound, "already_exists": codes.AlreadyExists, "conflict": codes.Aborted, "cancelled": codes.Canceled, "canceled": codes.Canceled, "deadline_exceeded": codes.DeadlineExceeded, "aborted": codes.Aborted, "failed_precondition": codes.FailedPrecondition, "unauthenticated": codes.Unauthenticated, "permission_denied": codes.PermissionDenied, "unavailable": codes.Unavailable, "resource_exhausted": codes.ResourceExhausted}[cp.Code(err)]
	if code == codes.OK {
		return status.Error(codes.Internal, "internal control-plane error")
	}
	return status.Error(code, err.Error())
}
func fromResource(r *pb.Resource) (cp.Resource, error) {
	if r == nil || r.Metadata == nil {
		return cp.Resource{}, status.Error(codes.InvalidArgument, "resource and metadata required")
	}
	m := r.Metadata
	if m.Id != "" || m.TenantId != "" || m.ProjectId != "" || m.Revision != 0 || m.Digest != "" || m.CreatedAt != nil || r.State != nil {
		return cp.Resource{}, status.Error(codes.InvalidArgument, "server-owned resource fields cannot be supplied")
	}
	var spec proto.Message
	switch r.Kind {
	case "Task":
		spec = r.GetTaskSpec()
	case "Workspace":
		spec = r.GetWorkspaceSpec()
	case "ModelConfig":
		spec = r.GetModelConfigSpec()
	case "Gateway":
		spec = r.GetGatewaySpec()
	default:
		return cp.Resource{}, status.Error(codes.InvalidArgument, "only definition resources may be created")
	}
	if spec == nil || !spec.ProtoReflect().IsValid() {
		return cp.Resource{}, status.Error(codes.InvalidArgument, "kind and typed spec must agree")
	}
	// Core definitions use strict Go JSON, whose int64 values are numbers.
	// protobuf JSON instead emits int64 strings, so encode the generated typed
	// definition's JSON-tagged fields directly without a float conversion.
	raw, err := json.Marshal(spec)
	if err != nil {
		return cp.Resource{}, err
	}
	return cp.Resource{Kind: r.Kind, Metadata: cp.Metadata{Name: m.Name, SchemaVersion: m.SchemaVersion}, Spec: raw}, nil
}
func resource(r cp.Resource) (*pb.Resource, error) {
	out := &pb.Resource{Kind: r.Kind, Metadata: &pb.Metadata{Id: r.Metadata.ID, TenantId: r.Metadata.TenantID, ProjectId: r.Metadata.ProjectID, Name: r.Metadata.Name, SchemaVersion: r.Metadata.SchemaVersion, Revision: r.Metadata.Revision, Digest: r.Metadata.Digest, CreatedAt: timestamppb.New(r.Metadata.CreatedAt)}}
	var target proto.Message
	switch r.Kind {
	case "Task":
		v := &pb.TaskSpec{}
		out.Spec = &pb.Resource_TaskSpec{TaskSpec: v}
		target = v
	case "Workspace":
		v := &pb.WorkspaceSpec{}
		out.Spec = &pb.Resource_WorkspaceSpec{WorkspaceSpec: v}
		target = v
	case "ModelConfig":
		v := &pb.ModelConfigSpec{}
		out.Spec = &pb.Resource_ModelConfigSpec{ModelConfigSpec: v}
		target = v
	case "Gateway":
		v := &pb.GatewaySpec{}
		out.Spec = &pb.Resource_GatewaySpec{GatewaySpec: v}
		target = v
	case "Operation":
		v := &pb.OperationSpec{}
		out.Spec = &pb.Resource_OperationSpec{OperationSpec: v}
		target = v
	case "Checkpoint":
		v := &pb.CheckpointSpec{}
		out.Spec = &pb.Resource_CheckpointSpec{CheckpointSpec: v}
		target = v
	case "Run":
		var v cp.RunSpec
		if err := json.Unmarshal(r.Spec, &v); err != nil {
			return nil, err
		}
		task, e := resource(v.Task)
		if e != nil {
			return nil, e
		}
		workspace, e := resource(v.Workspace)
		if e != nil {
			return nil, e
		}
		gateway, e := resource(v.Gateway)
		if e != nil {
			return nil, e
		}
		run := &pb.RunSpec{Task: task, Workspace: workspace, Gateway: gateway}
		for _, model := range v.Models {
			m, e := resource(model)
			if e != nil {
				return nil, e
			}
			run.Models = append(run.Models, m)
		}
		out.Spec = &pb.Resource_RunSpec{RunSpec: run}
	case "Branch":
		var v cp.BranchSpec
		if err := json.Unmarshal(r.Spec, &v); err != nil {
			return nil, err
		}
		model, e := resource(v.Model)
		if e != nil {
			return nil, e
		}
		out.Spec = &pb.Resource_BranchSpec{BranchSpec: &pb.BranchSpec{RunId: v.RunID, Model: model, ParentCheckpointId: v.ParentCheckpointID, ParentBranchId: v.ParentBranchID, ForkOperationId: v.ForkOperationID}}
	default:
		return nil, status.Error(codes.Internal, "unknown stored resource kind")
	}
	if target != nil {
		if err := protojson.Unmarshal(r.Spec, target); err != nil {
			return nil, err
		}
	}
	if len(r.Status) > 0 {
		switch r.Kind {
		case "Branch":
			st := &pb.BranchStatus{}
			if err := protojson.Unmarshal(r.Status, st); err != nil {
				return nil, err
			}
			out.State = &pb.Resource_BranchStatus{BranchStatus: st}
		case "Operation":
			st := &pb.OperationStatus{}
			if err := protojson.Unmarshal(r.Status, st); err != nil {
				return nil, err
			}
			out.State = &pb.Resource_OperationStatus{OperationStatus: st}
		}
	}
	return out, nil
}
func (s *Server) PutResource(ctx context.Context, req *pb.PutResourceRequest) (*pb.Resource, error) {
	sc, err := scope(ctx, req.ProjectId)
	if err != nil {
		return nil, err
	}
	r, err := fromResource(req.Resource)
	if err != nil {
		return nil, err
	}
	v, err := s.service.PutResource(ctx, sc, r, req.IdempotencyKey)
	if err != nil {
		return nil, canonical(err)
	}
	vout, err := resource(v)
	return vout, canonical(err)
}
func (s *Server) GetResource(ctx context.Context, req *pb.GetResourceRequest) (*pb.Resource, error) {
	sc, err := scope(ctx, req.ProjectId)
	if err != nil {
		return nil, err
	}
	v, err := s.service.GetResource(ctx, sc, req.Kind, req.Id)
	if err != nil {
		return nil, canonical(err)
	}
	vout, err := resource(v)
	return vout, canonical(err)
}
func (s *Server) ListResources(ctx context.Context, req *pb.ListResourcesRequest) (*pb.ListResourcesResponse, error) {
	sc, err := scope(ctx, req.ProjectId)
	if err != nil {
		return nil, err
	}
	vs, err := s.service.ListResources(ctx, sc, req.Kind, req.After, int(req.Limit))
	if err != nil {
		return nil, canonical(err)
	}
	out := &pb.ListResourcesResponse{}
	for _, v := range vs {
		r, e := resource(v)
		if e != nil {
			return nil, canonical(e)
		}
		out.Resources = append(out.Resources, r)
		out.NextAfter = r.Metadata.Id
	}
	return out, nil
}
func submission(v cp.Submission) (*pb.Submission, error) {
	r, e := resource(v.Run)
	if e != nil {
		return nil, e
	}
	b, e := resource(v.Branch)
	if e != nil {
		return nil, e
	}
	o, e := resource(v.Operation)
	if e != nil {
		return nil, e
	}
	return &pb.Submission{Run: r, Branch: b, Operation: o}, nil
}
func (s *Server) Submit(ctx context.Context, req *pb.SubmitRequest) (*pb.Submission, error) {
	sc, e := scope(ctx, req.ProjectId)
	if e != nil {
		return nil, e
	}
	v, e := s.service.Submit(ctx, sc, req.TaskId, req.IdempotencyKey)
	if e != nil {
		return nil, canonical(e)
	}
	out, e := submission(v)
	return out, canonical(e)
}
func (s *Server) Action(ctx context.Context, req *pb.ActionRequest) (*pb.Resource, error) {
	sc, e := scope(ctx, req.ProjectId)
	if e != nil {
		return nil, e
	}
	v, e := s.service.Action(ctx, sc, req.BranchId, req.Action, req.IdempotencyKey)
	if e != nil {
		return nil, canonical(e)
	}
	out, e := resource(v)
	return out, canonical(e)
}
func (s *Server) Events(ctx context.Context, req *pb.EventsRequest) (*pb.EventsResponse, error) {
	sc, e := scope(ctx, req.ProjectId)
	if e != nil {
		return nil, e
	}
	vs, e := s.service.Events(ctx, sc, req.BranchId, req.AfterSequence, int(req.Limit))
	if e != nil {
		return nil, canonical(e)
	}
	out := &pb.EventsResponse{NextSequence: req.AfterSequence}
	for _, v := range vs {
		var data map[string]any
		if e := json.Unmarshal(v.Data, &data); e != nil {
			return nil, canonical(e)
		}
		st, e := structpb.NewStruct(data)
		if e != nil {
			return nil, canonical(e)
		}
		out.Events = append(out.Events, &pb.Event{Id: v.ID, BranchId: v.BranchID, Sequence: v.Sequence, Epoch: v.Epoch, Type: v.Type, Data: st, CreatedAt: timestamppb.New(v.CreatedAt)})
		out.NextSequence = v.Sequence
	}
	return out, nil
}
func (s *Server) Exec(req *pb.ExecRequest, stream grpc.ServerStreamingServer[guest.ExecEvent]) error {
	sc, e := scope(stream.Context(), req.ProjectId)
	if e != nil {
		return e
	}
	if req.Command == nil {
		return status.Error(codes.InvalidArgument, "command required")
	}
	return canonical(s.service.Exec(stream.Context(), sc, req.BranchId, req.Command, stream.Send))
}
func (s *Server) ReadFile(ctx context.Context, req *pb.ReadFileRequest) (*guest.ReadFileResponse, error) {
	sc, e := scope(ctx, req.ProjectId)
	if e != nil {
		return nil, e
	}
	v, e := s.service.ReadFile(ctx, sc, req.BranchId, &guest.ReadFileRequest{Path: req.Path})
	return v, canonical(e)
}
func (s *Server) WriteFile(ctx context.Context, req *pb.WriteFileRequest) (*guest.WriteFileResponse, error) {
	sc, e := scope(ctx, req.ProjectId)
	if e != nil {
		return nil, e
	}
	v, e := s.service.WriteFile(ctx, sc, req.BranchId, &guest.WriteFileRequest{Path: req.Path, Data: req.Data})
	return v, canonical(e)
}
