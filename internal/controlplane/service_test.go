package controlplane_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	cp "github.com/gduchidze/hypercall/internal/controlplane"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAdmissionRejectsUnknownAndMutableInputs(t *testing.T) {
	svc := cp.NewService(nil, nil)
	scope := cp.Scope{TenantID: "tenant-a", ProjectID: "project-a"}
	for _, spec := range []string{`{"template_digest":"latest","command":["python"],"unknown":true}`, `{"template_digest":"latest","command":["python"]}`} {
		_, err := svc.PutResource(context.Background(), scope, cp.Resource{Kind: "Task", Metadata: cp.Metadata{Name: "task"}, Spec: json.RawMessage(spec)}, "key")
		if cp.Code(err) != "invalid_argument" {
			t.Fatalf("expected invalid_argument, got %v", err)
		}
	}
}

func TestDurableSubmissionIsIdempotentScopedAndOrdered(t *testing.T) {
	store, svc, scope := fixture(t)
	task := definitions(t, svc, scope)
	sub, err := svc.Submit(context.Background(), scope, task.Metadata.ID, "submit")
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := svc.Submit(context.Background(), scope, task.Metadata.ID, "submit")
	if err != nil || duplicate.Run.Metadata.ID != sub.Run.Metadata.ID {
		t.Fatalf("duplicate changed result: %v", err)
	}
	if _, err = svc.GetResource(context.Background(), cp.Scope{TenantID: "other", ProjectID: scope.ProjectID}, "Run", sub.Run.Metadata.ID); cp.Code(err) != "not_found" {
		t.Fatalf("cross tenant access: %v", err)
	}
	ev, err := svc.Events(context.Background(), scope, sub.Branch.Metadata.ID, 0, 10)
	if err != nil || len(ev) != 1 || ev[0].Sequence != 1 || ev[0].Type != "submitted" {
		t.Fatalf("events %#v %v", ev, err)
	}
	store.Close()
	reopened, err := cp.Open(context.Background(), testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err = reopened.GetResource(context.Background(), scope, "Run", sub.Run.Metadata.ID); err != nil {
		t.Fatalf("restart lost run: %v", err)
	}
}

var testDatabases sync.Map

func testDSN(t *testing.T) string {
	t.Helper()
	if value, ok := testDatabases.Load(t); ok {
		return value.(string)
	}
	dsn := os.Getenv("HYPERCALL_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("set HYPERCALL_TEST_POSTGRES for real PostgreSQL tests")
	}
	db, e := sql.Open("pgx", dsn)
	if e != nil {
		t.Fatal(e)
	}
	schema := fmt.Sprintf("test_%d", time.Now().UnixNano())
	if _, e = db.Exec("CREATE SCHEMA " + schema); e != nil {
		t.Fatal(e)
	}
	u, e := url.Parse(dsn)
	if e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	result := u.String()
	testDatabases.Store(t, result)
	t.Cleanup(func() { testDatabases.Delete(t); db.Exec("DROP SCHEMA " + schema + " CASCADE"); db.Close() })
	return result
}
func fixture(t *testing.T) (*cp.Store, *cp.Service, cp.Scope) {
	t.Helper()
	ctx := context.Background()
	st, e := cp.Open(ctx, testDSN(t))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { st.Close() })
	if e = st.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	return st, cp.NewService(st, cp.UnavailableBackend{}), cp.Scope{TenantID: t.Name() + strings.ReplaceAll(t.TempDir(), "/", ""), ProjectID: "project"}
}
func definitions(t *testing.T, svc *cp.Service, scope cp.Scope) cp.Resource {
	t.Helper()
	ctx := context.Background()
	pin := "sha256:" + strings.Repeat("a", 64)
	put := func(kind, name string, spec any) cp.Resource {
		raw, _ := json.Marshal(spec)
		r, e := svc.PutResource(ctx, scope, cp.Resource{Kind: kind, Metadata: cp.Metadata{Name: name}, Spec: raw}, "put-"+name)
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	ws := put("Workspace", "workspace", cp.WorkspaceSpec{PrivateWrites: true})
	model := put("ModelConfig", "model", cp.ModelConfigSpec{Provider: "fixture", Model: "test", Revision: "1", ArtifactDigest: pin})
	gw := put("Gateway", "gateway", cp.GatewaySpec{Routes: map[string]string{"fixture": "https://fixture.invalid"}})
	return put("Task", "task", cp.TaskSpec{Workspace: cp.Ref{ID: ws.Metadata.ID, Digest: ws.Metadata.Digest}, Models: []cp.Ref{{ID: model.Metadata.ID, Digest: model.Metadata.Digest}}, Gateway: cp.Ref{ID: gw.Metadata.ID, Digest: gw.Metadata.Digest}, TemplateDigest: pin, AgentDigest: pin, DatasetDigest: pin, Command: []string{"python", "agent.py"}, Limits: cp.Limits{CPU: 1, MemoryMB: 128, DiskMB: 128, TimeoutSeconds: 60}})
}

func TestAdmissionBoundsInputsAndRejectsDuplicateWorkspacePaths(t *testing.T) {
	svc := cp.NewService(nil, nil)
	scope := cp.Scope{TenantID: "tenant", ProjectID: "project"}
	pin := "sha256:" + strings.Repeat("a", 64)
	specs := []cp.Resource{
		{Kind: "Workspace", Metadata: cp.Metadata{Name: "workspace"}, Spec: json.RawMessage(`{"private_writes":true,"repositories":[{"url":"https://password@repo.invalid","commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`)},
		{Kind: "Workspace", Metadata: cp.Metadata{Name: "workspace"}, Spec: json.RawMessage(`{"private_writes":true,"files":[{"path":"same","content":"","digest":"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},{"path":"same","content":"","digest":"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}]}`)},
	}
	raw, _ := json.Marshal(cp.TaskSpec{Workspace: cp.Ref{ID: "ws", Digest: pin}, Models: []cp.Ref{{ID: "m", Digest: pin}}, Gateway: cp.Ref{ID: "gw", Digest: pin}, TemplateDigest: pin, AgentDigest: pin, DatasetDigest: pin, Command: []string{strings.Repeat("x", 64<<10)}, Limits: cp.Limits{CPU: 1, MemoryMB: 128, DiskMB: 128, TimeoutSeconds: 60}})
	specs = append(specs, cp.Resource{Kind: "Task", Metadata: cp.Metadata{Name: "task"}, Spec: raw})
	for _, r := range specs {
		if _, e := svc.PutResource(context.Background(), scope, r, "key"); cp.Code(e) != "invalid_argument" {
			t.Fatalf("unbounded/ambiguous input accepted: %v", e)
		}
	}
}

type advertisedSnapshots struct{ cp.UnavailableBackend }

func (advertisedSnapshots) Supports(string) bool { return true }
func TestResumeRequiresBackendSnapshotCapability(t *testing.T) {
	svc := cp.NewService(nil, cp.UnavailableBackend{})
	_, e := svc.Action(context.Background(), cp.Scope{TenantID: "tenant", ProjectID: "project"}, "branch", "resume", "key")
	if cp.Code(e) != "failed_precondition" {
		t.Fatalf("unsupported resume accepted: %v", e)
	}
}

func TestControlledProfileHasNoArbitraryCommandOrEnvironment(t *testing.T) {
	_, svc, scope := fixture(t)
	task := definitions(t, svc, scope)
	var spec cp.TaskSpec
	json.Unmarshal(task.Spec, &spec)
	spec.ExecutionMode = "controlled_python_v1"
	spec.Command = nil
	spec.Env = nil
	spec.ControlledCounter = 7
	raw, _ := json.Marshal(spec)
	controlled, e := svc.PutResource(context.Background(), scope, cp.Resource{Kind: "Task", Metadata: cp.Metadata{Name: "controlled"}, Spec: raw}, "controlled")
	if e != nil {
		t.Fatal(e)
	}
	var saved cp.TaskSpec
	json.Unmarshal(controlled.Spec, &saved)
	if saved.ExecutionMode != "controlled_python_v1" || saved.ControlledCounter != 7 {
		t.Fatal("controlled profile inputs were lost")
	}
	for _, mutate := range []func(*cp.TaskSpec){func(s *cp.TaskSpec) { s.Command = []string{"python", "other.py"} }, func(s *cp.TaskSpec) { s.Env = map[string]string{"USER_CODE": "x"} }, func(s *cp.TaskSpec) { s.ExecutionMode = "unknown" }, func(s *cp.TaskSpec) { s.ExecutionMode = "foreground_v1"; s.Command = []string{"python"} }} {
		bad := spec
		mutate(&bad)
		raw, _ = json.Marshal(bad)
		if _, e = svc.PutResource(context.Background(), scope, cp.Resource{Kind: "Task", Metadata: cp.Metadata{Name: "bad-profile"}, Spec: raw}, "bad"); cp.Code(e) != "invalid_argument" {
			t.Fatalf("unsupported profile configuration accepted: %v", e)
		}
	}
}

func TestControlledProfileCanonicalizesEmptyCommandRepresentations(t *testing.T) {
	_, svc, scope := fixture(t)
	task := definitions(t, svc, scope)
	var spec cp.TaskSpec
	json.Unmarshal(task.Spec, &spec)
	spec.ExecutionMode = "controlled_python_v1"
	spec.Command = nil
	spec.Env = nil
	raw, _ := json.Marshal(spec)
	first, e := svc.PutResource(context.Background(), scope, cp.Resource{Kind: "Task", Metadata: cp.Metadata{Name: "controlled"}, Spec: raw}, "one")
	if e != nil {
		t.Fatal(e)
	}
	spec.Command = []string{}
	spec.Env = map[string]string{}
	raw, _ = json.Marshal(spec)
	second, e := svc.PutResource(context.Background(), scope, cp.Resource{Kind: "Task", Metadata: cp.Metadata{Name: "controlled"}, Spec: raw}, "two")
	if e != nil || second.Metadata.ID != first.Metadata.ID {
		t.Fatalf("empty command encoding changed immutable controlled task: %v", e)
	}
}
