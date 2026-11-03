package hostapi_test

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"os"
	"testing"
	"time"

	publicv1 "github.com/gduchidze/hypercall/gen/go/hypercall/v1"
	"github.com/gduchidze/hypercall/internal/api"
	"github.com/gduchidze/hypercall/internal/controlplane"
	"github.com/gduchidze/hypercall/internal/hostapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

func TestPostgresPublicControlledSuspendResumeUsesRetainedNativeManifestAndFreshAllocation(t *testing.T) {
	dsn := os.Getenv("HYPERCALL_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("set HYPERCALL_TEST_POSTGRES for composed controlled snapshot conformance")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	admin, e := sql.Open("pgx", dsn)
	if e != nil {
		t.Fatal(e)
	}
	schema := fmt.Sprintf("snapshot_public_%d", time.Now().UnixNano())
	if _, e = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { admin.Exec("DROP SCHEMA " + schema + " CASCADE"); admin.Close() })
	u, e := url.Parse(dsn)
	if e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	dsn = u.String()
	store, e := controlplane.Open(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	if e = store.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	bridge, _, a, _ := controlledSetup(t)
	serverTLS, clientTLS, _ := certificateConfigs(t)
	hostServer, e := hostapi.NewServer(bridge, serverTLS, "spiffe://hypercall/controller")
	if e != nil {
		t.Fatal(e)
	}
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	go hostServer.Serve(listener)
	defer listener.Close()
	defer hostServer.Stop()
	conn, e := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	backend := hostapi.NewClient(conn, hostapi.ClientOptions{ControlledSnapshots: true})
	token := "controlled-public-test-key-32-characters"
	auth, e := api.NewAuthenticator(map[string]api.Principal{token: {TenantID: a.Scope.TenantID, Projects: []string{a.Scope.ProjectID}}})
	if e != nil {
		t.Fatal(e)
	}
	pub := api.NewServer(controlplane.NewService(store, backend), auth)
	local := bufconn.Listen(4 << 20)
	go pub.GRPC().Serve(local)
	defer pub.GRPC().Stop()
	defer local.Close()
	publicConn, e := grpc.NewClient("passthrough:///controlled-public", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return local.Dial() }))
	if e != nil {
		t.Fatal(e)
	}
	defer publicConn.Close()
	client := publicv1.NewControlPlaneClient(publicConn)
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	manifest := `apiVersion: hypercall.io/v1
kind: Workspace
metadata: {name: workspace}
spec: {private_writes: true}
---
apiVersion: hypercall.io/v1
kind: ModelConfig
metadata: {name: model}
spec: {provider: fixture, model: controlled, revision: v1, artifact_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}
---
apiVersion: hypercall.io/v1
kind: Gateway
metadata: {name: gateway}
spec: {routes: {fixture: 'https://fixture.invalid/v1'}}
---
apiVersion: hypercall.io/v1
kind: Task
metadata: {name: controlled}
spec:
 workspace: {id: workspace}
 models: [{id: model}]
 gateway: {id: gateway}
 template_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
 agent_digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
 dataset_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
 execution_mode: controlled_python_v1
 controlled_counter: 7
 limits: {cpu: 1, memory_mb: 256, disk_mb: 8, timeout_seconds: 60}
`
	submission, e := client.SubmitManifest(ctx, &publicv1.SubmitManifestRequest{ProjectId: a.Scope.ProjectID, ManifestYaml: manifest, IdempotencyKey: "submit"})
	if e != nil {
		t.Fatal(e)
	}
	branchID := submission.Branch.Metadata.Id
	controller := &controlplane.Controller{Store: store, Backend: backend, WorkerID: "snapshot-public-before"}
	wait := func(want string) *publicv1.Resource {
		t.Helper()
		for {
			e := controller.Tick(ctx)
			if e != nil && controlplane.Code(e) != "not_found" && controlplane.Code(e) != "aborted" {
				t.Fatal(e)
			}
			r, e := client.GetResource(ctx, &publicv1.GetResourceRequest{ProjectId: a.Scope.ProjectID, Kind: "Branch", Id: branchID})
			if e != nil {
				t.Fatal(e)
			}
			if r.GetBranchStatus().Observed == want {
				return r
			}
			if ctx.Err() != nil {
				t.Fatalf("branch failed to reach %s: %v", want, r)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	initial := wait("running")
	if _, e = client.Action(ctx, &publicv1.ActionRequest{ProjectId: a.Scope.ProjectID, BranchId: branchID, Action: "suspend", IdempotencyKey: "suspend"}); e != nil {
		t.Fatal(e)
	}
	wait("suspended")
	checkpoints, e := client.ListResources(ctx, &publicv1.ListResourcesRequest{ProjectId: a.Scope.ProjectID, Kind: "Checkpoint"})
	if e != nil || len(checkpoints.GetResources()) != 1 {
		t.Fatalf("retained checkpoint: %v %v", checkpoints, e)
	}
	cp := checkpoints.Resources[0].GetCheckpointSpec()
	if cp.NativeCheckpointId == "" || len(cp.SnapshotManifest) == 0 || !cp.Verified {
		t.Fatalf("native metadata not retained through public/Postgres API: %v", cp)
	}
	// Reopen the database/controller owner after suspension; continuation must use
	// the retained native checkpoint, not the task's cold-start Ensure path.
	if e = store.Close(); e != nil {
		t.Fatal(e)
	}
	store, e = controlplane.Open(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	controller = &controlplane.Controller{Store: store, Backend: backend, WorkerID: "snapshot-public-after"}
	service := controlplane.NewService(store, backend)
	// Public API's original Store connection was deliberately closed; restart it
	// against the reopened database while preserving authentication and host client.
	pub.GRPC().Stop()
	pub = api.NewServer(service, auth)
	newLocal := bufconn.Listen(4 << 20)
	go pub.GRPC().Serve(newLocal)
	defer pub.GRPC().Stop()
	defer newLocal.Close()
	newConn, e := grpc.NewClient("passthrough:///controlled-public-restart", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return newLocal.Dial() }))
	if e != nil {
		t.Fatal(e)
	}
	defer newConn.Close()
	client = publicv1.NewControlPlaneClient(newConn)
	if _, e = client.Action(ctx, &publicv1.ActionRequest{ProjectId: a.Scope.ProjectID, BranchId: branchID, Action: "resume", IdempotencyKey: "resume"}); e != nil {
		t.Fatal(e)
	}
	resumed := wait("running")
	if resumed.GetBranchStatus().SandboxId == initial.GetBranchStatus().SandboxId || resumed.GetBranchStatus().ExecutionId == initial.GetBranchStatus().ExecutionId || resumed.GetBranchStatus().Epoch <= initial.GetBranchStatus().Epoch {
		t.Fatalf("resume reused identity: initial=%v resumed=%v", initial, resumed)
	}
	if _, e = client.Action(ctx, &publicv1.ActionRequest{ProjectId: a.Scope.ProjectID, BranchId: branchID, Action: "cancel", IdempotencyKey: "cancel"}); e != nil {
		t.Fatal(e)
	}
	wait("cancelled")
}
