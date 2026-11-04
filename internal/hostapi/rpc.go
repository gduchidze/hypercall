package hostapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	harnessv1 "github.com/gduchidze/hypercall/gen/go/hypercall/harness/v1"
	hostv1 "github.com/gduchidze/hypercall/gen/go/hypercall/host/v1"
	publicv1 "github.com/gduchidze/hypercall/gen/go/hypercall/v1"
	"github.com/gduchidze/hypercall/internal/controlplane"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func loadTLS(caFile, certFile, keyFile string) (*tls.Config, error) {
	pair, e := tls.LoadX509KeyPair(certFile, keyFile)
	if e != nil {
		return nil, e
	}
	pem, e := os.ReadFile(caFile)
	if e != nil {
		return nil, e
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("invalid CA PEM")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, RootCAs: pool, ClientCAs: pool}, nil
}
func ServerTLS(caFile, certFile, keyFile string) (*tls.Config, error) {
	c, e := loadTLS(caFile, certFile, keyFile)
	if e == nil {
		c.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return c, e
}
func ClientTLS(caFile, certFile, keyFile, serverName string) (*tls.Config, error) {
	c, e := loadTLS(caFile, certFile, keyFile)
	if e == nil {
		if serverName == "" {
			return nil, fmt.Errorf("host TLS server name required")
		}
		c.ServerName = serverName
	}
	return c, e
}

// NewServer allows one explicitly configured controller certificate URI and the
// bridge's fixed scope. A CA signature alone does not grant controller access.
func NewServer(b *Bridge, t *tls.Config, controllerURI string) (*grpc.Server, error) {
	if b == nil || t == nil || t.ClientAuth != tls.RequireAndVerifyClientCert || t.ClientCAs == nil || len(t.Certificates) == 0 || t.InsecureSkipVerify || controllerURI == "" {
		return nil, fmt.Errorf("verified mutual TLS and controller certificate URI required")
	}
	authorize := func(ctx context.Context) error {
		p, ok := peer.FromContext(ctx)
		if !ok {
			return status.Error(codes.Unauthenticated, "controller TLS identity required")
		}
		info, ok := p.AuthInfo.(credentials.TLSInfo)
		if !ok || len(info.State.VerifiedChains) == 0 || len(info.State.PeerCertificates) == 0 {
			return status.Error(codes.Unauthenticated, "verified controller TLS identity required")
		}
		for _, u := range info.State.PeerCertificates[0].URIs {
			if u.String() == controllerURI {
				return nil
			}
		}
		return status.Error(codes.PermissionDenied, "controller certificate URI is unauthorized")
	}
	tlsConfig := t.Clone()
	tlsConfig.MinVersion = tls.VersionTLS13
	s := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)), grpc.MaxRecvMsgSize(8<<20), grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		if e := authorize(ctx); e != nil {
			return nil, e
		}
		return next(ctx, req)
	}), grpc.StreamInterceptor(func(srv any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, next grpc.StreamHandler) error {
		if e := authorize(stream.Context()); e != nil {
			return e
		}
		return next(srv, stream)
	}))
	hostv1.RegisterHostServer(s, &server{bridge: b})
	return s, nil
}

type server struct {
	hostv1.UnimplementedHostServer
	bridge *Bridge
}

func fromWire(a *hostv1.Assignment) (controlplane.Assignment, error) {
	if a == nil {
		return controlplane.Assignment{}, fail("invalid_argument", "assignment required")
	}
	v := controlplane.Assignment{Scope: controlplane.Scope{TenantID: a.TenantId, ProjectID: a.ProjectId}, BranchID: a.BranchId, SandboxID: a.SandboxId, ExecutionID: a.ExecutionId, Epoch: a.Epoch, LeaseUntil: time.Unix(0, a.LeaseUntilUnixNano).UTC(), HarnessSession: a.HarnessSession, ParentCheckpointID: a.ParentCheckpointId, ParentBranchID: a.ParentBranchId, Barrier: barrierFromWire(a.Barrier)}
	if len(a.ResolvedRunJson) > 0 {
		if len(a.ResolvedRunJson) > 4<<20 || decode(a.ResolvedRunJson, &v.Run) != nil {
			return v, fail("invalid_argument", "invalid resolved run JSON")
		}
	}
	if len(a.SelectedModelJson) > 0 {
		if len(a.SelectedModelJson) > 1<<20 || decode(a.SelectedModelJson, &v.SelectedModel) != nil {
			return v, fail("invalid_argument", "invalid immutable selected model JSON")
		}
	}
	return v, nil
}
func wire(a controlplane.Assignment) *hostv1.Assignment {
	run, _ := json.Marshal(a.Run)
	selected, _ := json.Marshal(a.SelectedModel)
	return &hostv1.Assignment{TenantId: a.Scope.TenantID, ProjectId: a.Scope.ProjectID, BranchId: a.BranchID, SandboxId: a.SandboxID, ExecutionId: a.ExecutionID, Epoch: a.Epoch, LeaseUntilUnixNano: a.LeaseUntil.UnixNano(), ResolvedRunJson: run, HarnessSession: a.HarnessSession, SelectedModelJson: selected, ParentCheckpointId: a.ParentCheckpointID, ParentBranchId: a.ParentBranchID, Barrier: barrierWire(a.Barrier)}
}
func grpcError(e error) error {
	if e == nil {
		return nil
	}
	if _, ok := status.FromError(e); ok {
		return e
	}
	mapping := map[string]codes.Code{"invalid_argument": codes.InvalidArgument, "permission_denied": codes.PermissionDenied, "aborted": codes.Aborted, "not_found": codes.NotFound, "failed_precondition": codes.FailedPrecondition, "unavailable": codes.Unavailable, "cancelled": codes.Canceled, "deadline_exceeded": codes.DeadlineExceeded, "resource_exhausted": codes.ResourceExhausted}
	c, ok := mapping[controlplane.Code(e)]
	if !ok {
		c = codes.Internal
	}
	return status.Error(c, e.Error())
}
func clientError(e error) error {
	if e == nil {
		return nil
	}
	mapping := map[codes.Code]string{codes.InvalidArgument: "invalid_argument", codes.PermissionDenied: "permission_denied", codes.Unauthenticated: "unauthenticated", codes.Aborted: "aborted", codes.NotFound: "not_found", codes.FailedPrecondition: "failed_precondition", codes.Unavailable: "unavailable", codes.Canceled: "cancelled", codes.DeadlineExceeded: "deadline_exceeded", codes.ResourceExhausted: "resource_exhausted"}
	s := status.Convert(e)
	c, ok := mapping[s.Code()]
	if !ok {
		c = "internal"
	}
	return fail(c, s.Message())
}
func (s *server) call(ctx context.Context, a *hostv1.Assignment, fn func(context.Context, controlplane.Assignment) (controlplane.Observation, error)) (*hostv1.Observation, error) {
	v, e := fromWire(a)
	if e != nil {
		return nil, grpcError(e)
	}
	o, e := fn(ctx, v)
	if e != nil {
		return nil, grpcError(e)
	}
	return &hostv1.Observation{State: o.State, Checkpoint: checkpointWire(o.Checkpoint)}, nil
}
func (s *server) Ensure(ctx context.Context, a *hostv1.Assignment) (*hostv1.Observation, error) {
	return s.call(ctx, a, s.bridge.Ensure)
}
func (s *server) Observe(ctx context.Context, a *hostv1.Assignment) (*hostv1.Observation, error) {
	return s.call(ctx, a, s.bridge.Observe)
}
func (s *server) Stop(ctx context.Context, a *hostv1.Assignment) (*hostv1.Observation, error) {
	return s.call(ctx, a, s.bridge.Stop)
}
func (s *server) Exec(r *hostv1.ExecRequest, stream grpc.ServerStreamingServer[guestv1.ExecEvent]) error {
	a, e := fromWire(r.Assignment)
	if e != nil {
		return grpcError(e)
	}
	return grpcError(s.bridge.Exec(stream.Context(), a, r.Command, stream.Send))
}
func (s *server) ReadFile(ctx context.Context, r *hostv1.ReadFileRequest) (*guestv1.ReadFileResponse, error) {
	a, e := fromWire(r.Assignment)
	if e != nil {
		return nil, grpcError(e)
	}
	v, e := s.bridge.ReadFile(ctx, a, r.File)
	return v, grpcError(e)
}
func (s *server) WriteFile(ctx context.Context, r *hostv1.WriteFileRequest) (*guestv1.WriteFileResponse, error) {
	a, e := fromWire(r.Assignment)
	if e != nil {
		return nil, grpcError(e)
	}
	v, e := s.bridge.WriteFile(ctx, a, r.File)
	return v, grpcError(e)
}

type ClientOptions struct {
	ControlledSnapshots bool
	CooperativeAgents   bool
}
type Client struct {
	rpc     hostv1.HostClient
	options ClientOptions
}

func NewClient(conn grpc.ClientConnInterface, options ...ClientOptions) *Client {
	c := &Client{rpc: hostv1.NewHostClient(conn)}
	if len(options) > 0 {
		c.options = options[0]
	}
	return c
}
func (c *Client) Supports(action string) bool {
	return action != "suspend" && action != "resume" && action != "fork" || c.options.ControlledSnapshots || c.options.CooperativeAgents
}
func observation(v *hostv1.Observation, e error) (controlplane.Observation, error) {
	if e != nil {
		return controlplane.Observation{}, clientError(e)
	}
	return controlplane.Observation{State: v.State, Checkpoint: checkpointFromWire(v.Checkpoint)}, nil
}
func (c *Client) Ensure(ctx context.Context, a controlplane.Assignment) (controlplane.Observation, error) {
	v, e := c.rpc.Ensure(ctx, wire(a))
	return observation(v, e)
}
func (c *Client) Observe(ctx context.Context, a controlplane.Assignment) (controlplane.Observation, error) {
	v, e := c.rpc.Observe(ctx, wire(a))
	return observation(v, e)
}
func (c *Client) Stop(ctx context.Context, a controlplane.Assignment) (controlplane.Observation, error) {
	v, e := c.rpc.Stop(ctx, wire(a))
	return observation(v, e)
}
func (c *Client) SupportsRun(action string, run controlplane.RunSpec) bool {
	if action != "suspend" && action != "resume" && action != "fork" {
		return true
	}
	var t controlplane.TaskSpec
	if decode(run.Task.Spec, &t) != nil {
		return false
	}
	return c.options.ControlledSnapshots && t.ExecutionMode == "controlled_python_v1" || c.options.CooperativeAgents && t.ExecutionMode == "cooperative_python_v1"
}
func (c *Client) Suspend(ctx context.Context, a controlplane.Assignment) (controlplane.Observation, error) {
	if !c.options.ControlledSnapshots && !c.options.CooperativeAgents {
		return controlplane.Observation{}, fail("failed_precondition", "snapshot host adapter is not enabled")
	}
	v, e := c.rpc.Suspend(ctx, wire(a))
	return observation(v, e)
}
func (c *Client) Resume(ctx context.Context, a controlplane.Assignment, cp controlplane.CheckpointSpec) (controlplane.Observation, error) {
	if !c.options.ControlledSnapshots && !c.options.CooperativeAgents {
		return controlplane.Observation{}, fail("failed_precondition", "snapshot host adapter is not enabled")
	}
	v, e := c.rpc.Resume(ctx, &hostv1.ResumeRequest{Assignment: wire(a), Checkpoint: checkpointWire(&cp)})
	return observation(v, e)
}
func (c *Client) Exec(ctx context.Context, a controlplane.Assignment, r *guestv1.ExecRequest, emit func(*guestv1.ExecEvent) error) error {
	if emit == nil {
		return fail("invalid_argument", "event callback required")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, e := c.rpc.Exec(ctx, &hostv1.ExecRequest{Assignment: wire(a), Command: r})
	if e != nil {
		return clientError(e)
	}
	result := false
	for {
		v, e := stream.Recv()
		if e == io.EOF {
			if !result {
				return fail("internal", "host execution ended without result")
			}
			return nil
		}
		if e != nil {
			return clientError(e)
		}
		if result {
			return fail("internal", "host emitted output after result")
		}
		result = v.GetResult() != nil
		if e = emit(v); e != nil {
			return e
		}
	}
}
func (c *Client) ReadFile(ctx context.Context, a controlplane.Assignment, r *guestv1.ReadFileRequest) (*guestv1.ReadFileResponse, error) {
	v, e := c.rpc.ReadFile(ctx, &hostv1.ReadFileRequest{Assignment: wire(a), File: r})
	return v, clientError(e)
}
func (c *Client) WriteFile(ctx context.Context, a controlplane.Assignment, r *guestv1.WriteFileRequest) (*guestv1.WriteFileResponse, error) {
	v, e := c.rpc.WriteFile(ctx, &hostv1.WriteFileRequest{Assignment: wire(a), File: r})
	return v, clientError(e)
}

var _ controlplane.Backend = (*Client)(nil)

func checkpointWire(cp *controlplane.CheckpointSpec) *publicv1.CheckpointSpec {
	if cp == nil {
		return nil
	}
	return &publicv1.CheckpointSpec{Barrier: barrierWire(cp.Barrier), RouteDigest: cp.RouteDigest, BudgetRemainingMs: cp.BudgetRemainingMS, NativeCheckpointId: cp.NativeCheckpointID, SnapshotManifest: cp.SnapshotManifest, BranchId: cp.BranchID, Compatibility: cp.Compatibility, ManifestDigest: cp.ManifestDigest, ManifestUri: cp.ManifestURI, ProtocolVersion: cp.ProtocolVersion, EventPosition: cp.EventPosition, Verified: cp.Verified}
}
func checkpointFromWire(cp *publicv1.CheckpointSpec) *controlplane.CheckpointSpec {
	if cp == nil {
		return nil
	}
	return &controlplane.CheckpointSpec{Barrier: barrierFromWire(cp.Barrier), RouteDigest: cp.RouteDigest, BudgetRemainingMS: cp.BudgetRemainingMs, NativeCheckpointID: cp.NativeCheckpointId, SnapshotManifest: cp.SnapshotManifest, BranchID: cp.BranchId, Compatibility: cp.Compatibility, ManifestDigest: cp.ManifestDigest, ManifestURI: cp.ManifestUri, ProtocolVersion: cp.ProtocolVersion, EventPosition: cp.EventPosition, Verified: cp.Verified}
}
func (s *server) Suspend(ctx context.Context, a *hostv1.Assignment) (*hostv1.Observation, error) {
	return s.call(ctx, a, s.bridge.Suspend)
}
func (s *server) Resume(ctx context.Context, r *hostv1.ResumeRequest) (*hostv1.Observation, error) {
	a, e := fromWire(r.Assignment)
	if e != nil {
		return nil, grpcError(e)
	}
	cp := checkpointFromWire(r.Checkpoint)
	if cp == nil {
		return nil, status.Error(codes.InvalidArgument, "checkpoint required")
	}
	o, e := s.bridge.Resume(ctx, a, *cp)
	if e != nil {
		return nil, grpcError(e)
	}
	return &hostv1.Observation{State: o.State, Checkpoint: checkpointWire(o.Checkpoint)}, nil
}

func (c *Client) AgentPoll(ctx context.Context, a controlplane.Assignment) (controlplane.AgentPoll, error) {
	if !c.options.CooperativeAgents {
		return controlplane.AgentPoll{}, fail("failed_precondition", "cooperative host adapter is not enabled")
	}
	p, e := c.rpc.AgentPoll(ctx, wire(a))
	if e != nil {
		return controlplane.AgentPoll{}, clientError(e)
	}
	return pollFromWire(p), nil
}
func (c *Client) AgentDeliver(ctx context.Context, a controlplane.Assignment, d controlplane.AgentDelivery) error {
	if !c.options.CooperativeAgents {
		return fail("failed_precondition", "cooperative host adapter is not enabled")
	}
	_, e := c.rpc.AgentDeliver(ctx, &hostv1.HostAgentDelivery{Assignment: wire(a), Delivery: deliveryWire(d)})
	return clientError(e)
}
func (s *server) AgentPoll(ctx context.Context, a *hostv1.Assignment) (*harnessv1.AgentPoll, error) {
	v, e := fromWire(a)
	if e != nil {
		return nil, grpcError(e)
	}
	p, e := s.bridge.AgentPoll(ctx, v)
	if e != nil {
		return nil, grpcError(e)
	}
	return pollWire(p), nil
}
func (s *server) AgentDeliver(ctx context.Context, r *hostv1.HostAgentDelivery) (*guestv1.DeliverAgentResponse, error) {
	a, e := fromWire(r.Assignment)
	if e != nil {
		return nil, grpcError(e)
	}
	if e = s.bridge.AgentDeliver(ctx, a, deliveryFromWire(r.Delivery)); e != nil {
		return nil, grpcError(e)
	}
	return &guestv1.DeliverAgentResponse{}, nil
}
