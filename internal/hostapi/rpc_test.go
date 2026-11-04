package hostapi_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	publicv1 "github.com/gduchidze/hypercall/gen/go/hypercall/v1"
	"github.com/gduchidze/hypercall/internal/api"
	"github.com/gduchidze/hypercall/internal/controlplane"
	"github.com/gduchidze/hypercall/internal/guest"
	"github.com/gduchidze/hypercall/internal/hostapi"
	"github.com/gduchidze/hypercall/internal/sandbox"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

func TestMain(m *testing.M) { guest.RunExecHelper(); os.Exit(m.Run()) }

// guestRuntime is an explicit trusted-local system-boundary test driver. It uses
// the actual guest command/files/authentication implementation, without KVM.
type guestRuntime struct {
	runtimeDriver
	t       *testing.T
	client  guestv1.GuestClient
	session string
}

func (d *guestRuntime) Create(ctx context.Context, r sandbox.CreateRequest) (sandbox.Observation, error) {
	o, e := d.runtimeDriver.Create(ctx, r)
	if e != nil {
		return o, e
	}
	executable, e := os.Executable()
	if e != nil {
		return o, e
	}
	d.session = "host-private-test-credential-longer-than-32"
	service, e := guest.New(guest.Config{Workspace: d.t.TempDir(), Identity: &guestv1.AllocationIdentity{TenantId: r.Allocation.TenantID, ProjectId: r.Allocation.ProjectID, SandboxId: r.Allocation.SandboxID, ExecutionId: r.Allocation.ExecutionID, Epoch: r.Allocation.Epoch}, Credential: d.session, ExpiresAt: time.Now().Add(r.Limits.TTL), TrustedLocal: true, ExecHelper: executable, AllowedEnv: []string{"TZ"}})
	if e != nil {
		return o, e
	}
	listener := bufconn.Listen(2 << 20)
	server := grpc.NewServer()
	guestv1.RegisterGuestServer(server, service)
	go server.Serve(listener)
	conn, e := grpc.NewClient("passthrough:///guest-test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if e != nil {
		return o, e
	}
	d.client = guestv1.NewGuestClient(conn)
	d.t.Cleanup(func() { conn.Close(); server.Stop(); listener.Close(); service.Close() })
	return o, nil
}
func (d *guestRuntime) authorized(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+d.session)
}
func (d *guestRuntime) Exec(ctx context.Context, _ sandbox.Allocation, r *guestv1.ExecRequest, emit func(*guestv1.ExecEvent) error) error {
	stream, e := d.client.Exec(d.authorized(ctx), r)
	if e != nil {
		return e
	}
	for {
		ev, e := stream.Recv()
		if e == io.EOF {
			return nil
		}
		if e != nil {
			return e
		}
		if e = emit(ev); e != nil {
			return e
		}
	}
}
func (d *guestRuntime) ReadFile(ctx context.Context, _ sandbox.Allocation, r *guestv1.ReadFileRequest) (*guestv1.ReadFileResponse, error) {
	return d.client.ReadFile(d.authorized(ctx), r)
}
func (d *guestRuntime) WriteFile(ctx context.Context, _ sandbox.Allocation, r *guestv1.WriteFileRequest) (*guestv1.WriteFileResponse, error) {
	return d.client.WriteFile(d.authorized(ctx), r)
}
func certificateConfigs(t *testing.T) (*tls.Config, *tls.Config, *tls.Config) {
	t.Helper()
	dir := t.TempDir()
	pub, priv, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	raw, e := x509.CreateCertificate(rand.Reader, ca, ca, pub, priv)
	if e != nil {
		t.Fatal(e)
	}
	caPath := filepath.Join(dir, "ca.pem")
	if e = os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw}), 0600); e != nil {
		t.Fatal(e)
	}
	issue := func(name, uri string, server bool) (string, string) {
		p, k, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(int64(len(name) + 2)), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature}
		if server {
			leaf.DNSNames = []string{"host.test"}
			leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		} else {
			u, e := url.Parse(uri)
			if e != nil {
				t.Fatal(e)
			}
			leaf.URIs = []*url.URL{u}
			leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}
		cert, e := x509.CreateCertificate(rand.Reader, leaf, ca, p, priv)
		if e != nil {
			t.Fatal(e)
		}
		private, e := x509.MarshalPKCS8PrivateKey(k)
		if e != nil {
			t.Fatal(e)
		}
		certPath, keyPath := filepath.Join(dir, name+".pem"), filepath.Join(dir, name+".key")
		if e = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}), 0600); e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), 0600); e != nil {
			t.Fatal(e)
		}
		return certPath, keyPath
	}
	sc, sk := issue("server", "", true)
	cc, ck := issue("controller", "spiffe://hypercall/controller", false)
	oc, ok := issue("other", "spiffe://hypercall/other", false)
	server, e := hostapi.ServerTLS(caPath, sc, sk)
	if e != nil {
		t.Fatal(e)
	}
	client, e := hostapi.ClientTLS(caPath, cc, ck, "host.test")
	if e != nil {
		t.Fatal(e)
	}
	other, e := hostapi.ClientTLS(caPath, oc, ok, "host.test")
	if e != nil {
		t.Fatal(e)
	}
	return server, client, other
}
func TestAuthenticatedHostAppliesInputsExecutesTaskAndProxiesCommandsFiles(t *testing.T) {
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	a := assignment()
	var task controlplane.TaskSpec
	if e = json.Unmarshal(a.Run.Task.Spec, &task); e != nil {
		t.Fatal(e)
	}
	task.Command = []string{"/bin/sh", "-c", `cat main.py > artifact; printf '%s' "$TZ" >> artifact; sleep 30`}
	task.Env = map[string]string{"TZ": "UTC"}
	a.Run.Task.Spec, _ = json.Marshal(task)
	driver := &guestRuntime{runtimeDriver: runtimeDriver{files: map[string][]byte{}, executed: make(chan *guestv1.ExecRequest, 2)}, t: t}
	bridge, e := hostapi.NewBridge(hostapi.Config{Scope: a.Scope, StateDir: dir, TemplateDigest: task.TemplateDigest}, driver)
	if e != nil {
		t.Fatal(e)
	}
	serverTLS, clientTLS, otherTLS := certificateConfigs(t)
	server, e := hostapi.NewServer(bridge, serverTLS, "spiffe://hypercall/controller")
	if e != nil {
		t.Fatal(e)
	}
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	go server.Serve(listener)
	t.Cleanup(func() { server.Stop(); listener.Close(); bridge.Close() })
	dial := func(c *tls.Config) *hostapi.Client {
		conn, e := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(c)))
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { conn.Close() })
		return hostapi.NewClient(conn)
	}
	client := dial(clientTLS)
	other := dial(otherTLS)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, e = other.Ensure(ctx, a); controlplane.Code(e) != "permission_denied" {
		t.Fatalf("foreign certificate accepted: %v", e)
	}
	foreign := a
	foreign.Scope.ProjectID = "other"
	if _, e = client.Ensure(ctx, foreign); controlplane.Code(e) != "permission_denied" {
		t.Fatalf("foreign scope accepted: %v", e)
	}
	if o, e := client.Ensure(ctx, a); e != nil || o.State != "running" {
		t.Fatalf("ensure: %v %v", o, e)
	}
	for {
		v, e := client.ReadFile(ctx, a, &guestv1.ReadFileRequest{Path: "artifact"})
		if e == nil && string(v.Data) == "print('hello')UTC" {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("task did not consume workspace/env: %v %v", v, e)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, e = client.WriteFile(ctx, a, &guestv1.WriteFileRequest{Path: "request", Data: []byte("from client")}); e != nil {
		t.Fatal(e)
	}
	output := ""
	e = client.Exec(ctx, a, &guestv1.ExecRequest{Argv: []string{"/bin/cat", "request"}, TimeoutMs: 1000}, func(v *guestv1.ExecEvent) error { output += string(v.GetStdout()); return nil })
	if e != nil || output != "from client" {
		t.Fatalf("proxy execution %q: %v", output, e)
	}
	if o, e := client.Stop(ctx, a); e != nil || o.State != "stopped" {
		t.Fatalf("stop: %v %v", o, e)
	}
	if _, e = client.Ensure(ctx, a); controlplane.Code(e) != "aborted" {
		t.Fatalf("late Ensure accepted: %v", e)
	}
}

func TestPostgresPublicAPIControllerAuthenticatedHostAndGuestRecoverTogether(t *testing.T) {
	dsn := os.Getenv("HYPERCALL_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("set HYPERCALL_TEST_POSTGRES for composed real Postgres/host/guest integration")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	admin, e := sql.Open("pgx", dsn)
	if e != nil {
		t.Fatal(e)
	}
	schema := fmt.Sprintf("host_composed_%d", time.Now().UnixNano())
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
	t.Cleanup(func() { store.Close() })
	if e = store.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	scope := controlplane.Scope{TenantID: "tenant", ProjectID: "project"}
	driver := &guestRuntime{runtimeDriver: runtimeDriver{files: map[string][]byte{}, executed: make(chan *guestv1.ExecRequest, 2)}, t: t}
	template := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	bridge, e := hostapi.NewBridge(hostapi.Config{Scope: scope, StateDir: dir, TemplateDigest: template}, driver)
	if e != nil {
		t.Fatal(e)
	}
	serverTLS, clientTLS, _ := certificateConfigs(t)
	hostServer, e := hostapi.NewServer(bridge, serverTLS, "spiffe://hypercall/controller")
	if e != nil {
		t.Fatal(e)
	}
	hostListener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	go hostServer.Serve(hostListener)
	t.Cleanup(func() { hostServer.Stop(); hostListener.Close(); bridge.Close() })
	hostConn, e := grpc.NewClient(hostListener.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { hostConn.Close() })
	backend := hostapi.NewClient(hostConn)
	token := "public-composed-key-with-at-least-32-bytes"
	auth, e := api.NewAuthenticator(map[string]api.Principal{token: {TenantID: scope.TenantID, Projects: []string{scope.ProjectID}}})
	if e != nil {
		t.Fatal(e)
	}
	startPublic := func(s *controlplane.Store) (publicv1.ControlPlaneClient, *grpc.Server) {
		server := api.NewServer(controlplane.NewService(s, backend), auth)
		listener := bufconn.Listen(4 << 20)
		go server.GRPC().Serve(listener)
		conn, e := grpc.NewClient("passthrough:///public-composed", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { conn.Close(); server.GRPC().Stop(); listener.Close() })
		return publicv1.NewControlPlaneClient(conn), server.GRPC()
	}
	client, publicServer := startPublic(store)
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	python, e := exec.LookPath("python3")
	if e != nil {
		t.Fatal("python3 required for composed guest task")
	}
	argv, _ := json.Marshal([]string{python, "agent.py"})
	program := []byte("from pathlib import Path\nimport time\np = Path('starts')\np.write_text((p.read_text() if p.exists() else '') + 'start\\n')\nPath('ready').write_text('ready')\ntime.sleep(30)\n")
	digest := sha256.Sum256(program)
	manifest := fmt.Sprintf(`apiVersion: hypercall.io/v1
kind: Workspace
metadata: {name: workspace}
spec:
  private_writes: true
  files:
    - path: agent.py
      content: %s
      digest: sha256:%s
---
apiVersion: hypercall.io/v1
kind: ModelConfig
metadata: {name: model}
spec: {provider: fixture, model: local-test, revision: fixture-v1, artifact_digest: %s}
---
apiVersion: hypercall.io/v1
kind: Gateway
metadata: {name: gateway}
spec: {routes: {fixture: 'https://fixture.invalid/v1'}}
---
apiVersion: hypercall.io/v1
kind: Task
metadata: {name: task}
spec:
  workspace: {id: workspace}
  models: [{id: model}]
  gateway: {id: gateway}
  template_digest: %s
  agent_digest: %s
  dataset_digest: %s
  command: %s
  limits: {cpu: 1, memory_mb: 256, disk_mb: 8, timeout_seconds: 60}
`, base64.StdEncoding.EncodeToString(program), hex.EncodeToString(digest[:]), template, template, template, template, argv)
	submit := &publicv1.SubmitManifestRequest{ProjectId: scope.ProjectID, ManifestYaml: manifest, IdempotencyKey: "once"}
	submission, e := client.SubmitManifest(ctx, submit)
	if e != nil {
		t.Fatal(e)
	}
	branchID := submission.Branch.Metadata.Id
	controller := &controlplane.Controller{Store: store, Backend: backend, WorkerID: "controller-before-restart"}
	if e = controller.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	waitRead := func(c publicv1.ControlPlaneClient, name, want string) {
		t.Helper()
		for {
			r, e := c.ReadFile(ctx, &publicv1.ReadFileRequest{ProjectId: scope.ProjectID, BranchId: branchID, Path: name})
			if e == nil && string(r.Data) == want {
				return
			}
			if ctx.Err() != nil {
				t.Fatalf("read %s expected %q: %v %v", name, want, r, e)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitRead(client, "ready", "ready")
	waitRead(client, "starts", "start\n")
	// Restart the API/database connection and controller while the privileged host
	// and its task worker continue. The new controller must observe, never replay.
	publicServer.Stop()
	if e = store.Close(); e != nil {
		t.Fatal(e)
	}
	store, e = controlplane.Open(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	client, _ = startPublic(store)
	controller = &controlplane.Controller{Store: store, Backend: backend, WorkerID: "controller-after-restart"}
	duplicate, e := client.SubmitManifest(ctx, submit)
	if e != nil || duplicate.GetRun().GetMetadata().GetId() != submission.Run.Metadata.Id {
		t.Fatalf("duplicate after restart: %v %v", duplicate, e)
	}
	for {
		e = controller.Tick(ctx)
		if e == nil {
			break
		}
		if controlplane.Code(e) != "not_found" {
			t.Fatal(e)
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitRead(client, "starts", "start\n")
	branch, e := client.GetResource(ctx, &publicv1.GetResourceRequest{ProjectId: scope.ProjectID, Kind: "Branch", Id: branchID})
	if e != nil || branch.GetBranchStatus().Observed != "running" || branch.GetBranchStatus().Epoch != 1 {
		t.Fatalf("recovered branch: %v %v", branch, e)
	}
	if _, e = client.WriteFile(ctx, &publicv1.WriteFileRequest{ProjectId: scope.ProjectID, BranchId: branchID, Path: "request", Data: []byte("after restart")}); e != nil {
		t.Fatal(e)
	}
	stream, e := client.Exec(ctx, &publicv1.ExecRequest{ProjectId: scope.ProjectID, BranchId: branchID, Command: &guestv1.ExecRequest{Argv: []string{"/bin/cat", "request"}, TimeoutMs: 1000}})
	if e != nil {
		t.Fatal(e)
	}
	output := ""
	result := false
	for {
		event, e := stream.Recv()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		output += string(event.GetStdout())
		if r := event.GetResult(); r != nil {
			result = r.ExitCode == 0
		}
	}
	if output != "after restart" || !result {
		t.Fatalf("proxied command output %q result=%t", output, result)
	}
	if _, e = client.Action(ctx, &publicv1.ActionRequest{ProjectId: scope.ProjectID, BranchId: branchID, Action: "cancel", IdempotencyKey: "cancel-once"}); e != nil {
		t.Fatal(e)
	}
	for {
		e = controller.Tick(ctx)
		if e != nil && controlplane.Code(e) != "not_found" && controlplane.Code(e) != "aborted" {
			t.Fatal(e)
		}
		branch, e = client.GetResource(ctx, &publicv1.GetResourceRequest{ProjectId: scope.ProjectID, Kind: "Branch", Id: branchID})
		if e != nil {
			t.Fatal(e)
		}
		if branch.GetBranchStatus().Observed == "cancelled" {
			break
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(10 * time.Millisecond)
	}
	events, e := client.Events(ctx, &publicv1.EventsRequest{ProjectId: scope.ProjectID, BranchId: branchID})
	if e != nil {
		t.Fatal(e)
	}
	for i, event := range events.Events {
		if event.Sequence != int64(i+1) {
			t.Fatalf("event order: %v", events)
		}
	}
	if events.Events[len(events.Events)-1].Type != "cancelled" {
		t.Fatalf("missing confirmed cancellation event: %v", events)
	}
}
