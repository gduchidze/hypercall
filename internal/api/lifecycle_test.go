package api_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	guest "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	"github.com/gduchidze/hypercall/internal/api"
	cp "github.com/gduchidze/hypercall/internal/controlplane"
)

func TestPublicClientsRetainCheckpointStateAcrossFreshResumeAssignments(t *testing.T) {
	dsn := os.Getenv("HYPERCALL_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("set HYPERCALL_TEST_POSTGRES for real Postgres lifecycle conformance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("api_lifecycle_%d", time.Now().UnixNano())
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
	store, err := cp.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	project := "lifecycle-project"
	token := "lifecycle-test-only-key-01234567890123456789"
	auth, err := api.NewAuthenticator(map[string]api.Principal{token: {TenantID: "lifecycle-tenant", Projects: []string{project}}})
	if err != nil {
		t.Fatal(err)
	}
	backend := &snapshotStateConformanceBackend{live: map[string]conformanceExecution{}, snapshots: map[string]map[string][]byte{}}
	srv := api.NewServer(cp.NewService(store, backend), auth)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.GRPC().Serve(listener)
	t.Cleanup(srv.GRPC().Stop)
	controller := &cp.Controller{Store: store, Backend: backend, WorkerID: "public-state-conformance-controller"}
	controllerCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- controller.Run(controllerCtx) }()
	defer func() { stop(); <-done }()
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
	scenario := exec.CommandContext(ctx, filepath.Join(root, ".venv/bin/python"), "test/contract/public_lifecycle_scenario.py", listener.Addr().String(), project)
	scenario.Dir = root
	scenario.Env = append(os.Environ(), "HYPERCALL_API_KEY="+token, "HYPERCALL_TEST_CLI="+cli)
	if output, err := scenario.CombinedOutput(); err != nil {
		t.Fatalf("public lifecycle: %v\n%s", err, output)
	} else {
		t.Log(string(output))
	}
}

// This fixture copies only Go byte maps. It qualifies durable control-plane and
// client protocol behavior; it never implements or claims VM snapshots, disks,
// agent barriers, real credential refresh, or host isolation.
type snapshotStateConformanceBackend struct {
	cp.UnavailableBackend
	mu        sync.Mutex
	live      map[string]conformanceExecution
	snapshots map[string]map[string][]byte
}
type conformanceExecution struct {
	assignment cp.Assignment
	files      map[string][]byte
}

func (*snapshotStateConformanceBackend) Supports(action string) bool {
	return action == "suspend" || action == "resume"
}
func cloneState(input map[string][]byte) map[string][]byte {
	out := map[string][]byte{}
	for path, data := range input {
		out[path] = append([]byte(nil), data...)
	}
	return out
}
func (b *snapshotStateConformanceBackend) Ensure(_ context.Context, a cp.Assignment) (cp.Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if previous, ok := b.live[a.BranchID]; ok {
		if previous.assignment.ExecutionID != a.ExecutionID || previous.assignment.Epoch != a.Epoch {
			return cp.Observation{}, &cp.Error{Kind: "aborted", Message: "test state already assigned"}
		}
		return cp.Observation{State: "running"}, nil
	}
	b.live[a.BranchID] = conformanceExecution{a, map[string][]byte{}}
	return cp.Observation{State: "running"}, nil
}
func (b *snapshotStateConformanceBackend) Observe(_ context.Context, a cp.Assignment) (cp.Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err := b.execution(a); err != nil {
		return cp.Observation{}, err
	}
	return cp.Observation{State: "running"}, nil
}
func (b *snapshotStateConformanceBackend) Stop(_ context.Context, a cp.Assignment) (cp.Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if current, ok := b.live[a.BranchID]; ok && (current.assignment.ExecutionID != a.ExecutionID || current.assignment.Epoch != a.Epoch) {
		return cp.Observation{}, &cp.Error{Kind: "aborted", Message: "stale test stop"}
	}
	delete(b.live, a.BranchID)
	return cp.Observation{State: "stopped"}, nil
}
func (b *snapshotStateConformanceBackend) execution(a cp.Assignment) (conformanceExecution, error) {
	e, ok := b.live[a.BranchID]
	if !ok || e.assignment.Scope != a.Scope || e.assignment.ExecutionID != a.ExecutionID || e.assignment.Epoch != a.Epoch || !time.Now().Before(a.LeaseUntil) {
		return conformanceExecution{}, &cp.Error{Kind: "failed_precondition", Message: "test execution is fenced"}
	}
	return e, nil
}
func (b *snapshotStateConformanceBackend) Suspend(_ context.Context, a cp.Assignment) (cp.Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, err := b.execution(a)
	if err != nil {
		return cp.Observation{}, err
	}
	raw, err := json.Marshal(e.files)
	if err != nil {
		return cp.Observation{}, err
	}
	sum := sha256.Sum256(raw)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	b.snapshots[digest] = cloneState(e.files)
	var task cp.TaskSpec
	if err = json.Unmarshal(a.Run.Task.Spec, &task); err != nil {
		return cp.Observation{}, err
	}
	delete(b.live, a.BranchID)
	return cp.Observation{State: "suspended", Checkpoint: &cp.CheckpointSpec{BranchID: a.BranchID, ManifestDigest: digest, ManifestURI: "test-only://snapshots/" + hex.EncodeToString(sum[:]), ProtocolVersion: "hypercall.snapshot.v1", Verified: true, Compatibility: map[string]string{"snapshot_format": "test-only-state-v1", "architecture": "test-only-no-vm", "template_digest": task.TemplateDigest}}}, nil
}
func (b *snapshotStateConformanceBackend) Resume(_ context.Context, a cp.Assignment, checkpoint cp.CheckpointSpec) (cp.Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	files, ok := b.snapshots[checkpoint.ManifestDigest]
	if !ok || checkpoint.BranchID != a.BranchID || !checkpoint.Verified {
		return cp.Observation{}, &cp.Error{Kind: "failed_precondition", Message: "test checkpoint unavailable"}
	}
	if current, ok := b.live[a.BranchID]; ok {
		if current.assignment.ExecutionID == a.ExecutionID && current.assignment.Epoch == a.Epoch {
			return cp.Observation{State: "running"}, nil
		}
		return cp.Observation{}, &cp.Error{Kind: "aborted", Message: "test execution conflict"}
	}
	b.live[a.BranchID] = conformanceExecution{a, cloneState(files)}
	return cp.Observation{State: "running"}, nil
}
func (b *snapshotStateConformanceBackend) ReadFile(_ context.Context, a cp.Assignment, req *guest.ReadFileRequest) (*guest.ReadFileResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, err := b.execution(a)
	if err != nil {
		return nil, err
	}
	return &guest.ReadFileResponse{Data: append([]byte(nil), e.files[req.Path]...)}, nil
}
func (b *snapshotStateConformanceBackend) WriteFile(_ context.Context, a cp.Assignment, req *guest.WriteFileRequest) (*guest.WriteFileResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, err := b.execution(a)
	if err != nil {
		return nil, err
	}
	e.files[req.Path] = append([]byte(nil), req.Data...)
	return &guest.WriteFileResponse{BytesWritten: uint64(len(req.Data))}, nil
}
