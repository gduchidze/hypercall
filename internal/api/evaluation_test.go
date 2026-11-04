package api_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/gduchidze/hypercall/gen/go/hypercall/v1"
	"github.com/gduchidze/hypercall/internal/api"
	cp "github.com/gduchidze/hypercall/internal/controlplane"
	"github.com/gduchidze/hypercall/internal/eval"
	"github.com/gduchidze/hypercall/internal/scorer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestPublicEvaluationRetainsReportsAndRejectsCrossProjectAccess(t *testing.T) {
	dsn := os.Getenv("HYPERCALL_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("HYPERCALL_TEST_POSTGRES required")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("eval_api_%d", time.Now().UnixNano())
	if _, err = admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec("DROP SCHEMA " + schema + " CASCADE")
	u, _ := url.Parse(dsn)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	st, err := cp.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err = st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	svc := cp.NewService(st, cp.UnavailableBackend{})
	key := "01234567890123456789012345678901"
	auth, _ := api.NewAuthenticator(map[string]api.Principal{key: {TenantID: "tenant", Projects: []string{"project"}}})
	server := api.NewServer(svc, auth)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go server.GRPC().Serve(listener)
	defer server.GRPC().Stop()
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := pb.NewControlPlaneClient(conn)
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+key)
	config := `apiVersion: hypercall.io/eval/v1
scope: whole_task
workload_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
candidates: [{id: fixture, model_digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb}]
cases: [{id: case, assert: [{type: equals, value: hello}]}]
repetitions: 1
`
	plan, _ := eval.Compile([]byte(config))
	output := "hello"
	admitted, err := client.SubmitEvaluation(ctx, &pb.SubmitEvaluationRequest{ProjectId: "project", ConfigYaml: config, IdempotencyKey: "eval", Observations: []*pb.EvaluationObservation{{CellId: plan.Cells[0].ID, Source: "synthetic", State: "complete", Output: &output, TraceComplete: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.GetEvaluation(ctx, &pb.GetEvaluationRequest{ProjectId: "other", Id: admitted.Id}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("project escaped: %v", err)
	}
	if err = svc.WorkEvaluation(context.Background(), "api-worker", time.Second, nil); err != nil {
		t.Fatal(err)
	}
	report, err := client.GetReport(ctx, &pb.GetReportRequest{ProjectId: "project", Id: admitted.Id})
	if err != nil {
		t.Fatal(err)
	}
	var decoded eval.Report
	if err = json.Unmarshal(report.ReportJson, &decoded); err != nil || !decoded.CI.Pass || !report.CiPass || report.Markdown == "" {
		t.Fatalf("bad report %+v %v", report, err)
	}
	workerCtx, stop := context.WithCancel(context.Background())
	defer stop()
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		for workerCtx.Err() == nil {
			_ = svc.WorkEvaluation(workerCtx, "scenario", time.Second, nil)
			time.Sleep(time.Millisecond)
		}
	}()
	binary := filepath.Join(t.TempDir(), "hypercall")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/hypercall")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v %s", err, output)
	}
	command := exec.Command("uv", "run", "--frozen", "python", "../../test/contract/public_evaluation_scenario.py", listener.Addr().String(), binary)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("public clients: %v %s", err, output)
	}
	stop()
	<-workerDone
	if image := os.Getenv("HYPERCALL_SCORER_IMAGE"); image != "" {
		code := []byte("def score(request):\n    return {'pass': request['output'] == 'hello', 'score': 1 if request['output'] == 'hello' else 0, 'reason':'synthetic fixture', 'cost': {'amount':0,'currency':'USD','kind':'synthetic','source':'fixture'}}\n")
		path := filepath.Join(t.TempDir(), "scorer.py")
		if err = os.WriteFile(path, code, 0600); err != nil {
			t.Fatal(err)
		}
		digest := fmt.Sprintf("sha256:%x", sha256.Sum256(code))
		gradedConfig := strings.Replace(config, "type: equals, value: hello", "type: python, scorer_digest: "+digest, 1)
		gradedPlan, err := eval.Compile([]byte(gradedConfig))
		if err != nil {
			t.Fatal(err)
		}
		admission, err := client.SubmitEvaluation(ctx, &pb.SubmitEvaluationRequest{ProjectId: "project", ConfigYaml: gradedConfig, IdempotencyKey: "real-isolated-grading", Observations: []*pb.EvaluationObservation{{CellId: gradedPlan.Cells[0].ID, Source: "synthetic", State: "complete", Output: &output, TraceComplete: true}}})
		if err != nil {
			t.Fatal(err)
		}
		graders := eval.IsolatedScorer{digest: scorer.NewRunner(scorer.Config{ImageDigest: image, CodeDigest: digest, CodePath: path, OwnerID: fmt.Sprintf("api-eval-%d", time.Now().UnixNano()), CPU: .5, MemoryMB: 128, PIDs: 64, Timeout: 10 * time.Second})}
		if err = svc.WorkEvaluation(context.Background(), "isolated-api-worker", time.Second, graders); err != nil {
			t.Fatal(err)
		}
		gradedReport, err := client.GetReport(ctx, &pb.GetReportRequest{ProjectId: "project", Id: admission.Id})
		if err != nil || !gradedReport.CiPass {
			t.Fatalf("composed isolated API grading %+v %v", gradedReport, err)
		}
	}
}
