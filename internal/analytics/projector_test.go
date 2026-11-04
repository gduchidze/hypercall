package analytics_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gduchidze/hypercall/internal/analytics"
	cp "github.com/gduchidze/hypercall/internal/controlplane"
	"github.com/gduchidze/hypercall/internal/eval"
)

func projectionDatabase(t *testing.T) (*sql.DB, *cp.Service, cp.Scope, *cp.Store) {
	t.Helper()
	dsn := os.Getenv("HYPERCALL_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("HYPERCALL_TEST_POSTGRES is not set")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("projection_%d", time.Now().UnixNano())
	if _, err = admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	store, err := cp.Open(context.Background(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); store.Close(); admin.Exec("DROP SCHEMA " + schema + " CASCADE"); admin.Close() })
	return db, cp.NewService(store, cp.UnavailableBackend{}), cp.Scope{TenantID: schema, ProjectID: "project"}, store
}

const projectionEvaluation = `apiVersion: hypercall.io/eval/v1
scope: whole_task
workload_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
candidates: [{id: fixture, model_digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb}]
cases: [{id: greeting, assert: [{type: equals, value: hello}]}]
repetitions: 1
`

func TestProjectionOutagePreservesReportsAndRecoversAfterRestart(t *testing.T) {
	real := clickhouse(t)
	db, svc, scope, _ := projectionDatabase(t)
	ctx := context.Background()
	plan, err := eval.Compile([]byte(projectionEvaluation))
	if err != nil {
		t.Fatal(err)
	}
	output := "hello"
	evaluation, err := svc.SubmitEvaluation(ctx, scope, projectionEvaluation, []eval.Observation{{CellID: plan.Cells[0].ID, Output: &output, State: "complete", Source: "synthetic", TraceComplete: true}}, "projection-eval")
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.WorkEvaluation(ctx, "projection-test", time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse(os.Getenv("HYPERCALL_TEST_CLICKHOUSE_URL"))
	proxy := httputil.NewSingleHostReverseProxy(target)
	var healthy atomic.Bool
	var ambiguous atomic.Bool
	proxy.ModifyResponse = func(r *http.Response) error {
		if ambiguous.CompareAndSwap(true, false) {
			r.StatusCode = http.StatusServiceUnavailable
		}
		return nil
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	remote, err := analytics.NewClickHouse(analytics.ClickHouseConfig{Endpoint: server.URL, Database: os.Getenv("HYPERCALL_TEST_CLICKHOUSE_DATABASE"), Username: os.Getenv("HYPERCALL_TEST_CLICKHOUSE_USER"), Password: os.Getenv("HYPERCALL_TEST_CLICKHOUSE_PASSWORD"), AllowInsecureLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	projector := analytics.NewProjector(db, remote)
	if _, err = projector.ProjectOnce(ctx); err == nil {
		t.Fatal("outage was not reported")
	}
	status, err := projector.Status(ctx, scope)
	if err != nil || status.Pending != 1 || status.OldestPendingSeconds <= 0 || status.LastDeliveredAt != nil {
		t.Fatalf("outage lag: %+v %v", status, err)
	}
	report, err := svc.GetReport(ctx, scope, evaluation.ID)
	if err != nil || !report.CI.Pass {
		t.Fatalf("analytics outage affected authoritative report: %+v %v", report, err)
	}
	healthy.Store(true)
	// The server commits an INSERT, then the response is lost. Receipt must remain
	// pending and retry must be logically deduplicated by the real ClickHouse.
	ambiguous.Store(true)
	if _, err = projector.ProjectOnce(ctx); err == nil {
		t.Fatal("ambiguous delivery was acknowledged")
	}
	projector = analytics.NewProjector(db, remote)
	if worked, err := projector.ProjectOnce(ctx); err != nil || !worked {
		t.Fatalf("restart catchup: %v %v", worked, err)
	}
	status, err = projector.Status(ctx, scope)
	if err != nil || status.Pending != 0 || status.LastDeliveredAt == nil {
		t.Fatalf("catchup lag: %+v %v", status, err)
	}
	events, err := real.Events(ctx, scope, 10)
	if err != nil || len(events) != 1 || events[0].ResourceID != evaluation.ID || events[0].Kind != "evaluation.completed" {
		t.Fatalf("projection contents: %+v %v", events, err)
	}
	other, err := projector.Status(ctx, cp.Scope{TenantID: "other", ProjectID: scope.ProjectID})
	if err != nil || other.Pending != 0 || other.LastDeliveredAt != nil {
		t.Fatalf("lag crossed scope: %+v %v", other, err)
	}
	if err = projector.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	if worked, err := projector.ProjectOnce(ctx); err != nil || !worked {
		t.Fatalf("rebuild: %v %v", worked, err)
	}
	events, err = real.Events(ctx, scope, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("rebuild changed logical projection: %+v %v", events, err)
	}
}

func TestBranchProjectionIsIndependentOfLifecycleConsumer(t *testing.T) {
	sink := clickhouse(t)
	db, svc, scope, store := projectionDatabase(t)
	ctx := context.Background()
	put := func(kind, name string, spec any) cp.Resource {
		raw, _ := json.Marshal(spec)
		r, err := svc.PutResource(ctx, scope, cp.Resource{Kind: kind, Metadata: cp.Metadata{Name: name}, Spec: raw}, "put-"+name)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	pin := "sha256:" + strings.Repeat("a", 64)
	ws := put("Workspace", "workspace", cp.WorkspaceSpec{PrivateWrites: true})
	model := put("ModelConfig", "model", cp.ModelConfigSpec{Provider: "fixture", Model: "test", Revision: "1", ArtifactDigest: pin})
	gw := put("Gateway", "gateway", cp.GatewaySpec{Routes: map[string]string{"fixture": "https://fixture.invalid"}})
	task := put("Task", "task", cp.TaskSpec{Workspace: cp.Ref{ID: ws.Metadata.ID, Digest: ws.Metadata.Digest}, Models: []cp.Ref{{ID: model.Metadata.ID, Digest: model.Metadata.Digest}}, Gateway: cp.Ref{ID: gw.Metadata.ID, Digest: gw.Metadata.Digest}, TemplateDigest: pin, AgentDigest: pin, DatasetDigest: pin, Command: []string{"python", "agent.py"}, Limits: cp.Limits{CPU: 1, MemoryMB: 128, DiskMB: 128, TimeoutSeconds: 60}})
	submission, err := svc.Submit(ctx, scope, task.Metadata.ID, "submit")
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := store.ClaimOutbox(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.AckOutbox(ctx, delivery); err != nil {
		t.Fatal(err)
	}
	projector := analytics.NewProjector(db, sink)
	status, err := projector.Status(ctx, scope)
	if err != nil || status.Pending != 1 {
		t.Fatalf("other consumer hid pending branch event: %+v %v", status, err)
	}
	if worked, err := projector.ProjectOnce(ctx); err != nil || !worked {
		t.Fatalf("branch projection: %v %v", worked, err)
	}
	projected, err := sink.Events(ctx, scope, 10)
	if err != nil || len(projected) != 1 || projected[0].EventID != "branch:"+delivery.Event.ID || projected[0].ResourceID != submission.Branch.Metadata.ID || projected[0].Kind != "submitted" {
		t.Fatalf("branch projection lost envelope: %+v %v", projected, err)
	}
	if err = projector.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	// Resetting analytics receipts never resets a separate consumer's receipts.
	if _, err = store.ClaimOutbox(ctx, time.Minute); cp.Code(err) != "not_found" {
		t.Fatalf("rebuild touched lifecycle receipts: %v", err)
	}
}

func TestRebuildFencesAnInFlightProjectionReceipt(t *testing.T) {
	real := clickhouse(t)
	db, svc, scope, _ := projectionDatabase(t)
	ctx := context.Background()
	plan, err := eval.Compile([]byte(projectionEvaluation))
	if err != nil {
		t.Fatal(err)
	}
	output := "hello"
	_, err = svc.SubmitEvaluation(ctx, scope, projectionEvaluation, []eval.Observation{{CellID: plan.Cells[0].ID, Output: &output, State: "complete", Source: "synthetic", TraceComplete: true}}, "fenced-eval")
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.WorkEvaluation(ctx, "projection-test", time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse(os.Getenv("HYPERCALL_TEST_CLICKHOUSE_URL"))
	proxy := httputil.NewSingleHostReverseProxy(target)
	inserted := make(chan struct{})
	release := make(chan struct{})
	var first atomic.Bool
	first.Store(true)
	proxy.ModifyResponse = func(*http.Response) error {
		if first.CompareAndSwap(true, false) {
			close(inserted)
			<-release
		}
		return nil
	}
	server := httptest.NewServer(proxy)
	defer server.Close()
	remote, err := analytics.NewClickHouse(analytics.ClickHouseConfig{Endpoint: server.URL, Database: os.Getenv("HYPERCALL_TEST_CLICKHOUSE_DATABASE"), Username: os.Getenv("HYPERCALL_TEST_CLICKHOUSE_USER"), Password: os.Getenv("HYPERCALL_TEST_CLICKHOUSE_PASSWORD"), AllowInsecureLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	projector := analytics.NewProjector(db, remote)
	completed := make(chan error, 1)
	go func() { _, err := projector.ProjectOnce(ctx); completed <- err }()
	select {
	case <-inserted:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("projection did not reach ClickHouse")
	}
	if worked, claimErr := projector.ProjectOnce(ctx); claimErr != nil || worked {
		close(release)
		t.Fatalf("concurrent worker stole live claim: %v %v", worked, claimErr)
	}
	if err = projector.Rebuild(ctx); err != nil {
		close(release)
		t.Fatal(err)
	}
	worked, err := projector.ProjectOnce(ctx)
	close(release)
	if err != nil || !worked {
		t.Fatalf("replacement claim: %v %v", worked, err)
	}
	if err = <-completed; err == nil {
		t.Fatal("superseded receipt was acknowledged")
	}
	status, err := projector.Status(ctx, scope)
	if err != nil || status.Pending != 0 {
		t.Fatalf("stale claim changed receipt: %+v %v", status, err)
	}
	events, err := real.Events(ctx, scope, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("stale attempt duplicated logical event: %+v %v", events, err)
	}
}
