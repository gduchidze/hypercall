package guest_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	"github.com/gduchidze/hypercall/internal/guest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

const credential = "test-session-secret-with-at-least-32-bytes"

func TestMain(m *testing.M) {
	guest.RunExecHelper()
	os.Exit(m.Run())
}

func TestBinaryFileRoundtripAndRejectedWritePreservesDestination(t *testing.T) {
	client, _ := startGuest(t, nil)
	data := []byte{0, 255, 1, 128, '\n'}
	written, err := client.WriteFile(authorized(t), &guestv1.WriteFileRequest{Path: "binary.dat", Data: data})
	if err != nil || written.GetBytesWritten() != 5 {
		t.Fatalf("write=%v err=%v", written, err)
	}
	if _, err := client.WriteFile(authorized(t), &guestv1.WriteFileRequest{Path: "binary.dat", Data: make([]byte, (1<<20)+1)}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("oversize write: %v", err)
	}
	read, err := client.ReadFile(authorized(t), &guestv1.ReadFileRequest{Path: "binary.dat"})
	if err != nil || !bytes.Equal(read.GetData(), data) {
		t.Fatalf("read=%v err=%v", read, err)
	}
}

func TestFileAndCwdPathsCannotEscapeWorkspace(t *testing.T) {
	client, workspace := startGuest(t, nil)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(workspace, "escape-file")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../secret", filepath.Join(outside, "secret"), "escape/secret", "escape-file"} {
		if _, err := client.ReadFile(authorized(t), &guestv1.ReadFileRequest{Path: path}); err == nil {
			t.Fatalf("read escape accepted: %q", path)
		}
		if _, err := client.WriteFile(authorized(t), &guestv1.WriteFileRequest{Path: path, Data: []byte("replace")}); err == nil {
			t.Fatalf("write escape accepted: %q", path)
		}
	}
	_, _, _, err := collect(t, client, authorized(t), &guestv1.ExecRequest{Argv: []string{"/bin/pwd"}, Cwd: "escape", TimeoutMs: 1000})
	if err == nil {
		t.Fatal("cwd symlink escape accepted")
	}
	// Verify the outside object through a separately rooted guest RPC adapter.
	outsideClient, _ := startGuest(t, func(cfg *guest.Config) { cfg.Workspace = outside })
	read, err := outsideClient.ReadFile(authorized(t), &guestv1.ReadFileRequest{Path: "secret"})
	if err != nil || string(read.GetData()) != "outside" {
		t.Fatalf("outside destination changed: %v %v", read, err)
	}
}

func TestCancelledAndDeadlineCommandsReapAndStopTheirGroups(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(strconv.FormatBool(deadline), func(t *testing.T) {
			client, _ := startGuest(t, nil)
			ctx, cancel := context.WithCancel(authorized(t))
			if deadline {
				ctx, cancel = context.WithTimeout(authorized(t), 100*time.Millisecond)
			}
			defer cancel()
			stream, err := client.Exec(ctx, &guestv1.ExecRequest{Argv: []string{"/bin/sh", "-c", `printf '%s\n' "$$"; (sleep 0.4; printf leaked > leaked) & wait`}, TimeoutMs: 2000})
			if err != nil {
				t.Fatal(err)
			}
			first, err := stream.Recv()
			if err != nil {
				t.Fatal(err)
			}
			pid := strings.TrimSpace(string(first.GetStdout()))
			if _, err := strconv.Atoi(pid); err != nil {
				t.Fatalf("not a process pid: %q", pid)
			}
			if !deadline {
				cancel()
			}
			for {
				_, err = stream.Recv()
				if err != nil {
					break
				}
			}
			want := codes.Canceled
			if deadline {
				want = codes.DeadlineExceeded
			}
			if status.Code(err) != want {
				t.Fatalf("cancellation: %v", err)
			}
			_, _, result, err := collect(t, client, authorized(t), &guestv1.ExecRequest{Argv: []string{"/bin/sh", "-c", "sleep 0.5; kill -0 " + pid + " 2>/dev/null && exit 42; test ! -e leaked"}, TimeoutMs: 1500})
			if err != nil || result == nil || result.ExitCode != 0 {
				t.Fatalf("process survived cancellation: result=%v err=%v", result, err)
			}
		})
	}
}

func TestOldSessionDoesNotAuthorizeFreshAllocation(t *testing.T) {
	client, _ := startGuest(t, func(cfg *guest.Config) {
		cfg.Identity.ExecutionId = "execution-two"
		cfg.Identity.Epoch = 8
		cfg.Credential = "fresh-session-secret-with-at-least-32-bytes"
	})
	if _, err := client.GetInfo(authorized(t), &guestv1.GetInfoRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("old credential: %v", err)
	}
	ctx := metadata.AppendToOutgoingContext(t.Context(), "authorization", "Bearer fresh-session-secret-with-at-least-32-bytes")
	info, err := client.GetInfo(ctx, &guestv1.GetInfoRequest{})
	if err != nil || info.GetIdentity().GetEpoch() != 8 || info.GetIdentity().GetExecutionId() != "execution-two" {
		t.Fatalf("fresh allocation: %v %v", info, err)
	}
}

func startGuest(t *testing.T, configure func(*guest.Config)) (guestv1.GuestClient, string) {
	t.Helper()
	workspace := t.TempDir()
	cfg := guest.Config{Workspace: workspace, Identity: &guestv1.AllocationIdentity{
		TenantId: "tenant-one", ProjectId: "project-one", SandboxId: "sandbox-one", ExecutionId: "execution-one", Epoch: 7,
	}, Credential: credential, ExpiresAt: time.Now().Add(time.Hour), TrustedLocal: true}
	cfg.ExecHelper, _ = os.Executable()
	if configure != nil {
		configure(&cfg)
	}
	svc, err := guest.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	guestv1.RegisterGuestServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///guest", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return guestv1.NewGuestClient(conn), workspace
}

func collect(t *testing.T, client guestv1.GuestClient, ctx context.Context, req *guestv1.ExecRequest) (string, string, *guestv1.ExecResult, error) {
	t.Helper()
	stream, err := client.Exec(ctx, req)
	if err != nil {
		return "", "", nil, err
	}
	var stdout, stderr string
	var result *guestv1.ExecResult
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			return stdout, stderr, result, nil
		}
		if err != nil {
			return stdout, stderr, result, err
		}
		if result != nil {
			t.Fatal("event received after terminal result")
		}
		stdout += string(event.GetStdout())
		stderr += string(event.GetStderr())
		if event.GetResult() != nil {
			result = event.GetResult()
		}
	}
}

func TestExecStreamsSeparateOutputAndNonzeroResult(t *testing.T) {
	client, _ := startGuest(t, nil)
	out, stderr, result, err := collect(t, client, authorized(t), &guestv1.ExecRequest{Argv: []string{"/bin/sh", "-c", "printf hello; printf problem >&2; exit 17"}, TimeoutMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello" || stderr != "problem" || result == nil || result.ExitCode != 17 || result.TimedOut {
		t.Fatalf("unexpected command: out=%q err=%q result=%v", out, stderr, result)
	}
}

func TestExecTimeoutAndOutputBudgetAreTerminalResults(t *testing.T) {
	client, _ := startGuest(t, nil)
	_, _, result, err := collect(t, client, authorized(t), &guestv1.ExecRequest{Argv: []string{"/bin/sh", "-c", "sleep 30"}, TimeoutMs: 50})
	if err != nil || result == nil || !result.TimedOut || result.ExitCode == 0 {
		t.Fatalf("timeout: result=%v err=%v", result, err)
	}
	out, _, result, err := collect(t, client, authorized(t), &guestv1.ExecRequest{Argv: []string{"/usr/bin/yes"}, TimeoutMs: 2000})
	if err != nil || len(out) > 1<<20 || result == nil || !result.OutputTruncated {
		t.Fatalf("output limit: bytes=%d result=%v err=%v", len(out), result, err)
	}
}

func TestExecEnvironmentIsExplicitAndBounded(t *testing.T) {
	t.Setenv("HYPERCALL_TEST_SECRET", "must-not-leak")
	client, _ := startGuest(t, func(cfg *guest.Config) { cfg.AllowedEnv = []string{"DEMO"} })
	out, _, result, err := collect(t, client, authorized(t), &guestv1.ExecRequest{Argv: []string{"/usr/bin/env"}, Env: map[string]string{"DEMO": "hello"}, TimeoutMs: 1000})
	if err != nil || result == nil || !strings.Contains(out, "DEMO=hello\n") || strings.Contains(out, "HYPERCALL_TEST_SECRET") {
		t.Fatalf("environment: %q %v %v", out, result, err)
	}
	for _, req := range []*guestv1.ExecRequest{
		{Argv: []string{"/bin/echo"}},
		{Argv: []string{"/bin/echo"}, TimeoutMs: 1000, Cwd: "../outside"},
		{Argv: []string{"/bin/echo"}, TimeoutMs: 1000, Env: map[string]string{"LD_PRELOAD": "bad"}},
		{Argv: []string{"/bin/echo", strings.Repeat("x", 32769)}, TimeoutMs: 1000},
	} {
		_, _, _, err := collect(t, client, authorized(t), req)
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("invalid exec accepted: %v", err)
		}
	}
}

func authorized(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+credential)
}

func TestGetInfoRequiresBoundSession(t *testing.T) {
	client, _ := startGuest(t, nil)
	for _, values := range [][]string{nil, {"Bearer wrong"}, {"Bearer " + credential, "Bearer " + credential}} {
		ctx := metadata.NewOutgoingContext(t.Context(), metadata.MD{"authorization": values})
		if _, err := client.GetInfo(ctx, &guestv1.GetInfoRequest{}); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("auth values count %d: %v", len(values), err)
		}
	}
	info, err := client.GetInfo(authorized(t), &guestv1.GetInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if info.GetIdentity().GetEpoch() != 7 || info.GetIdentity().GetTenantId() != "tenant-one" || info.GetProtocolVersion() != "hypercall.guest.v1" {
		t.Fatalf("unexpected allocation: %v", info)
	}
	if info.GetMaxFileBytes() != 1<<20 {
		t.Fatalf("unexpected file budget: %v", info)
	}
}

func TestEveryGuestOperationRejectsMissingSession(t *testing.T) {
	client, _ := startGuest(t, nil)
	if _, err := client.ReadFile(t.Context(), &guestv1.ReadFileRequest{Path: "missing"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("read auth: %v", err)
	}
	if _, err := client.WriteFile(t.Context(), &guestv1.WriteFileRequest{Path: "missing", Data: []byte("x")}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("write auth: %v", err)
	}
	_, _, _, err := collect(t, client, t.Context(), &guestv1.ExecRequest{Argv: []string{"/bin/echo", "x"}, TimeoutMs: 1000})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("exec auth: %v", err)
	}
}

func TestSessionExpiryStopsAdmittedCommandAndRejectsFurtherRequests(t *testing.T) {
	client, _ := startGuest(t, func(cfg *guest.Config) { cfg.ExpiresAt = time.Now().Add(200 * time.Millisecond) })
	_, _, result, err := collect(t, client, authorized(t), &guestv1.ExecRequest{Argv: []string{"/bin/sh", "-c", "sleep 30"}, TimeoutMs: 2000})
	if err != nil || result == nil || !result.TimedOut || result.ExitCode == 0 {
		t.Fatalf("session expiry: result=%v err=%v", result, err)
	}
	if _, err := client.GetInfo(authorized(t), &guestv1.GetInfoRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expired session: %v", err)
	}
}
