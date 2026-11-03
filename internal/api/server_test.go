package api_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	guest "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	pb "github.com/gduchidze/hypercall/gen/go/hypercall/v1"
	"github.com/gduchidze/hypercall/internal/api"
	cp "github.com/gduchidze/hypercall/internal/controlplane"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

func TestAuthenticatedIdentityCannotReadAnotherProject(t *testing.T) {
	auth, err := api.NewAuthenticator(map[string]api.Principal{"01234567890123456789012345678901": {TenantID: "tenant-a", Projects: []string{"project-a"}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := api.NewServer(nil, auth)
	listener := bufconn.Listen(1 << 20)
	go srv.GRPC().Serve(listener)
	t.Cleanup(srv.GRPC().Stop)
	conn, err := grpc.NewClient("passthrough:///test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := pb.NewControlPlaneClient(conn)
	req := &pb.GetResourceRequest{ProjectId: "project-b", Kind: "Task", Id: "secret"}
	if _, err = client.GetResource(context.Background(), req); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unauthenticated read: %v", err)
	}
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer 01234567890123456789012345678901")
	if _, err = client.GetResource(ctx, req); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("cross-project read: %v", err)
	}
	_, err = client.PutResource(ctx, &pb.PutResourceRequest{ProjectId: "project-a", Resource: &pb.Resource{Kind: "Task", Metadata: &pb.Metadata{Name: "spoof", TenantId: "tenant-b"}}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("client-supplied tenant identity: %v", err)
	}
	stream, err := client.Exec(ctx, &pb.ExecRequest{ProjectId: "project-b", BranchId: "secret"})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("cross-project streaming command: %v", err)
	}
	duplicate := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer 01234567890123456789012345678901")
	if _, err = client.GetResource(duplicate, req); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("duplicate authorization: %v", err)
	}
	httpServer := httptest.NewServer(srv.HTTP(conn))
	defer httpServer.Close()
	httpReq, _ := http.NewRequest("GET", httpServer.URL+"/v1/projects/project-b/resources/Task/secret", nil)
	httpReq.Header.Set("Authorization", "Bearer 01234567890123456789012345678901")
	response, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("REST project access: %d", response.StatusCode)
	}
}

func TestYAMLSubmissionPersistsResolvedInputsAcrossAPIRestart(t *testing.T) {
	dsn := os.Getenv("HYPERCALL_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("set HYPERCALL_TEST_POSTGRES for real Postgres public API integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("api_test_%d", time.Now().UnixNano())
	if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DROP SCHEMA " + schema + " CASCADE"); admin.Close() })
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	dsn = u.String()
	store, err := cp.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	project := fmt.Sprintf("api-%d", time.Now().UnixNano())
	token := "api-test-key-0123456789012345678901234567"
	otherToken := "other-tenant-key-01234567890123456789012345"
	auth, err := api.NewAuthenticator(map[string]api.Principal{token: {TenantID: "api-test-tenant", Projects: []string{project}}, otherToken: {TenantID: "other-tenant", Projects: []string{project}}})
	if err != nil {
		t.Fatal(err)
	}
	backend := &publicConformanceBackend{files: map[string][]byte{}}
	start := func() (*grpc.ClientConn, *api.Server) {
		srv := api.NewServer(cp.NewService(store, backend), auth)
		lis := bufconn.Listen(4 << 20)
		go srv.GRPC().Serve(lis)
		conn, e := grpc.NewClient("passthrough:///api", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }))
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { conn.Close(); srv.GRPC().Stop() })
		return conn, srv
	}
	conn, srv := start()
	client := pb.NewControlPlaneClient(conn)
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	manifest, err := os.ReadFile("../../test/fixtures/phase2a/task.yaml")
	if err != nil {
		t.Fatal(err)
	}
	request := &pb.SubmitManifestRequest{ProjectId: project, ManifestYaml: string(manifest), IdempotencyKey: "submit-once"}
	submitted, err := client.SubmitManifest(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := client.SubmitManifest(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if submitted.Run.Metadata.Id != replay.Run.Metadata.Id {
		t.Fatal("duplicate submission created another run")
	}
	if submitted.Run.GetRunSpec().Workspace.Metadata.Digest == "" || submitted.Run.GetRunSpec().Task.GetTaskSpec().Workspace.Id == "workspace" {
		t.Fatal("inputs were not resolved to immutable resources")
	}
	// Public protobuf int64 counters must reach strict core JSON as exact integers,
	// including values beyond JSON's floating-point exact range.
	controlled := proto.Clone(submitted.Run.GetRunSpec().Task).(*pb.Resource)
	controlled.Metadata = &pb.Metadata{Name: "controlled-precision"}
	controlled.GetTaskSpec().ExecutionMode = "controlled_python_v1"
	controlled.GetTaskSpec().ControlledCounter = 9007199254740993
	controlled.GetTaskSpec().Command = nil
	controlled.GetTaskSpec().Env = nil
	saved, err := client.PutResource(ctx, &pb.PutResourceRequest{ProjectId: project, Resource: controlled, IdempotencyKey: "controlled-precision"})
	if err != nil {
		t.Fatal(err)
	}
	if saved.GetTaskSpec().ControlledCounter != 9007199254740993 {
		t.Fatalf("counter lost precision: %d", saved.GetTaskSpec().ControlledCounter)
	}
	otherCtx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+otherToken)
	if _, err = client.GetResource(otherCtx, &pb.GetResourceRequest{ProjectId: project, Kind: "Run", Id: submitted.Run.Metadata.Id}); status.Code(err) != codes.NotFound {
		t.Fatalf("cross-tenant resource visibility: %v", err)
	}
	if _, err = client.Events(otherCtx, &pb.EventsRequest{ProjectId: project, BranchId: submitted.Branch.Metadata.Id}); status.Code(err) != codes.NotFound {
		t.Fatalf("cross-tenant event visibility: %v", err)
	}
	bad := strings.Replace(string(manifest), "private_writes: true", "private_writes: true\n  typo: true", 1)
	_, err = client.SubmitManifest(ctx, &pb.SubmitManifestRequest{ProjectId: project, ManifestYaml: bad, IdempotencyKey: "bad"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown spec field: %v", err)
	}
	srv.GRPC().Stop()
	conn2, srv2 := start()
	client2 := pb.NewControlPlaneClient(conn2)
	events, err := client2.Events(ctx, &pb.EventsRequest{ProjectId: project, BranchId: submitted.Branch.Metadata.Id})
	if err != nil {
		t.Fatal(err)
	}
	if len(events.Events) != 1 || events.Events[0].Type != "submitted" || events.NextSequence != 1 {
		t.Fatalf("durable events: %v", events)
	}
	empty, err := client2.Events(ctx, &pb.EventsRequest{ProjectId: project, BranchId: submitted.Branch.Metadata.Id, AfterSequence: events.NextSequence})
	if err != nil || len(empty.GetEvents()) != 0 {
		t.Fatalf("resume cursor: %v %v", empty, err)
	}
	httpServer := httptest.NewServer(srv2.HTTP(conn2))
	defer httpServer.Close()
	req, err := http.NewRequestWithContext(ctx, "GET", httpServer.URL+"/v1/projects/"+project+"/resources/Run/"+submitted.Run.Metadata.Id, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("generated REST inspect: %d", response.StatusCode)
	}
	// These clients use a real TCP gRPC connection to this Postgres-backed service.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv2.GRPC().Serve(listener)
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(t.TempDir(), "hypercall")
	build := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin/go"), "build", "-o", cli, "./cmd/hypercall")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	scenario := exec.CommandContext(ctx, filepath.Join(root, ".venv/bin/python"), "test/contract/public_client_scenario.py", listener.Addr().String(), project)
	scenario.Dir = root
	scenario.Env = append(os.Environ(), "HYPERCALL_API_KEY="+token, "HYPERCALL_TEST_CLI="+cli)
	if output, err := scenario.CombinedOutput(); err != nil {
		t.Fatalf("public SDK/CLI scenario: %v\n%s", err, output)
	} else {
		t.Log(string(output))
	}
	// Qualify transport marshalling/routing with a named test-only backend.
	// No process or VM execution is claimed by this fixture.
	controller := &cp.Controller{Store: store, Backend: backend, WorkerID: "public-conformance-test"}
	for i := 0; i < 10; i++ {
		if err = controller.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		branch, e := client2.GetResource(ctx, &pb.GetResourceRequest{ProjectId: project, Kind: "Branch", Id: submitted.Branch.Metadata.Id})
		if e != nil {
			t.Fatal(e)
		}
		if branch.GetBranchStatus().Observed == "running" {
			break
		}
		if i == 9 {
			t.Fatal("test backend did not reconcile running branch")
		}
	}
	commands := exec.CommandContext(ctx, filepath.Join(root, ".venv/bin/python"), "test/contract/public_command_scenario.py", listener.Addr().String(), project, submitted.Branch.Metadata.Id)
	commands.Dir = root
	commands.Env = append(os.Environ(), "HYPERCALL_API_KEY="+token)
	if output, err := commands.CombinedOutput(); err != nil {
		t.Fatalf("SDK command/file contract: %v\n%s", err, output)
	} else {
		t.Log(string(output))
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", httpServer.URL+"/v1/projects/"+project+"/branches/"+submitted.Branch.Metadata.Id+"/commands", strings.NewReader(`{"command":{"argv":["fixture"],"timeout_ms":1000}}`))
	if err != nil {
		t.Fatal(err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	httpResponse, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	defer httpResponse.Body.Close()
	body, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		t.Fatal(err)
	}
	if httpResponse.StatusCode != 200 || !strings.Contains(string(body), `"stdout":"Zml4dHVyZSBvdXRwdXQA"`) || !strings.Contains(string(body), `"exit_code":7`) {
		t.Fatalf("generated REST command stream: %d %s", httpResponse.StatusCode, body)
	}
}

// publicConformanceBackend is protocol test infrastructure, not sandbox runtime.
type publicConformanceBackend struct {
	cp.UnavailableBackend
	mu    sync.Mutex
	files map[string][]byte
}

func (*publicConformanceBackend) Ensure(context.Context, cp.Assignment) (cp.Observation, error) {
	return cp.Observation{State: "running"}, nil
}
func (*publicConformanceBackend) Exec(ctx context.Context, _ cp.Assignment, req *guest.ExecRequest, send func(*guest.ExecEvent) error) error {
	if len(req.Argv) > 0 {
		switch req.Argv[0] {
		case "wrapped-cancel":
			return &cp.Error{Kind: "cancelled", Message: "test-only wrapped cancellation"}
		case "wrapped-deadline":
			return &cp.Error{Kind: "deadline_exceeded", Message: "test-only wrapped deadline"}
		}
	}
	if len(req.Argv) > 0 && req.Argv[0] == "wait" {
		<-ctx.Done()
		return ctx.Err()
	}
	for _, event := range []*guest.ExecEvent{{Event: &guest.ExecEvent_Stdout{Stdout: []byte("fixture output\x00")}}, {Event: &guest.ExecEvent_Stderr{Stderr: []byte("fixture stderr\n")}}, {Event: &guest.ExecEvent_Result{Result: &guest.ExecResult{ExitCode: 7}}}} {
		if err := send(event); err != nil {
			return err
		}
	}
	return nil
}
func (b *publicConformanceBackend) ReadFile(_ context.Context, _ cp.Assignment, req *guest.ReadFileRequest) (*guest.ReadFileResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return &guest.ReadFileResponse{Data: append([]byte(nil), b.files[req.Path]...)}, nil
}
func (b *publicConformanceBackend) WriteFile(_ context.Context, _ cp.Assignment, req *guest.WriteFileRequest) (*guest.WriteFileResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.files[req.Path] = append([]byte(nil), req.Data...)
	return &guest.WriteFileResponse{BytesWritten: uint64(len(req.Data))}, nil
}
