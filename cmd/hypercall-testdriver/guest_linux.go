//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	"github.com/gduchidze/hypercall/internal/sandbox"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type session struct {
	Identity   *guestv1.AllocationIdentity `json:"identity"`
	Credential string                      `json:"credential"`
	ExpiresAt  time.Time                   `json:"expires_at"`
}

func (s *suite) session(a sandbox.Observation) (session, error) {
	paths, err := filepath.Glob(filepath.Join(s.o.StateDir, "primary", "allocations", "*", "bootstrap", "session.json"))
	if err != nil {
		return session{}, err
	}
	for _, p := range paths {
		data, e := os.ReadFile(p)
		if e != nil {
			return session{}, e
		}
		var v session
		if e = json.Unmarshal(data, &v); e != nil {
			return session{}, e
		}
		i := v.Identity
		if i != nil && i.TenantId == a.Allocation.TenantID && i.ProjectId == a.Allocation.ProjectID && i.SandboxId == a.Allocation.SandboxID && i.ExecutionId == a.Allocation.ExecutionID && i.Epoch == a.Allocation.Epoch {
			s.secrets = append(s.secrets, v.Credential)
			return v, nil
		}
	}
	return session{}, errors.New("trusted operator could not locate private bootstrap session")
}
func dialVSock(ctx context.Context, path string) (net.Conn, error) {
	return sandbox.DialVSock(ctx, path)
}
func rawClient(path string) (*grpc.ClientConn, error) {
	return grpc.NewClient("passthrough:///guest", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return dialVSock(ctx, path) }))
}
func info(ctx context.Context, path, credential string) (*guestv1.GetInfoResponse, error) {
	c, err := rawClient(path)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if credential != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+credential)
	}
	return guestv1.NewGuestClient(c).GetInfo(ctx, &guestv1.GetInfoRequest{})
}
func (s *suite) authentication(a sandbox.Observation) (map[string]any, error) {
	sess, err := s.session(a)
	if err != nil {
		return nil, err
	}
	for _, cred := range []string{"", "wrong-private-session-not-authorized"} {
		_, err := info(s.ctx, a.VSockPath, cred)
		if status.Code(err) != codes.Unauthenticated {
			return nil, fmt.Errorf("invalid credential returned %v, need Unauthenticated", status.Code(err))
		}
	}
	valid, err := info(s.ctx, a.VSockPath, sess.Credential)
	if err != nil {
		return nil, err
	}
	if valid.Identity.SandboxId != a.Allocation.SandboxID {
		return nil, errors.New("authenticated identity mismatch")
	}
	other, err := s.create(10 * time.Minute)
	if err != nil {
		return nil, err
	}
	otherSession, err := s.session(other)
	if err != nil {
		return nil, err
	}
	if _, err := info(s.ctx, a.VSockPath, otherSession.Credential); status.Code(err) != codes.Unauthenticated {
		return nil, fmt.Errorf("cross-allocation credential accepted or wrong status: %v", status.Code(err))
	}
	short, err := s.create(15 * time.Second)
	if err != nil {
		return nil, err
	}
	shortSession, err := s.session(short)
	if err != nil {
		return nil, err
	}
	wait := time.Until(short.ExpiresAt) + 150*time.Millisecond
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-s.ctx.Done():
			return nil, s.ctx.Err()
		case <-timer.C:
		}
	}
	_, err = info(s.ctx, short.VSockPath, shortSession.Credential)
	if err == nil {
		return nil, errors.New("expired session still authorized")
	}
	return map[string]any{"missing": "Unauthenticated", "wrong": "Unauthenticated", "cross_allocation": "Unauthenticated", "expired_status": status.Code(err).String(), "expiry_fences_allocation": true}, nil
}
func (s *suite) scopeEpoch(a sandbox.Observation) (map[string]any, error) {
	hostSecret := "qualification-host-only-bearer-never-forward"
	s.secrets = append(s.secrets, hostSecret)
	ctx := metadata.AppendToOutgoingContext(s.ctx, "authorization", "Bearer "+hostSecret)
	if _, err := s.b.ReadFile(ctx, a.Allocation, &guestv1.ReadFileRequest{Path: "binary.dat"}); err != nil {
		return nil, fmt.Errorf("backend did not replace caller credential: %w", err)
	}
	for _, kind := range []string{"tenant", "project", "epoch"} {
		other := a.Allocation
		switch kind {
		case "tenant":
			other.TenantID = "other-tenant"
		case "project":
			other.ProjectID = "other-project"
		case "epoch":
			other.Epoch++
		}
		if _, err := s.b.Inspect(s.ctx, other); !errors.Is(err, sandbox.ErrNotFound) {
			return nil, fmt.Errorf("%s mismatch: expected ErrNotFound, got %v", kind, err)
		}
	}
	return map[string]any{"tenant_rejected": true, "project_rejected": true, "epoch_rejected": true}, nil
}

func (s *suite) python(a sandbox.Observation) (map[string]any, error) {
	if !filepath.IsAbs(s.o.PythonProbe) {
		return nil, errors.New("--python-probe requires absolute scripts/qualify-python.py")
	}
	sess, err := s.session(a)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("/tmp", "hc-python-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "rpc.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	defer lis.Close()
	if err := os.Chmod(sock, 0600); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
	defer cancel()
	go func() {
		for {
			local, e := lis.Accept()
			if e != nil {
				return
			}
			go func() {
				defer local.Close()
				remote, e := dialVSock(ctx, a.VSockPath)
				if e != nil {
					return
				}
				defer remote.Close()
				done := make(chan struct{})
				go func() { io.Copy(remote, local); remote.Close(); close(done) }()
				io.Copy(local, remote)
				<-done
			}()
		}
	}()
	request, _ := json.Marshal(map[string]any{"endpoint": "unix:" + sock, "credential": sess.Credential, "identity": sess.Identity})
	cmd := exec.CommandContext(ctx, s.o.Python, s.o.PythonProbe)
	cmd.Stdin = strings.NewReader(string(request))
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "PYTHONPATH=" + os.Getenv("PYTHONPATH"), "PYTHONDONTWRITEBYTECODE=1"}
	var out, stderr safeBuffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("Python vsock probe failed: %w: %s", err, stderr.String())
	}
	var evidence map[string]any
	if err := json.Unmarshal([]byte(out.String()), &evidence); err != nil {
		return nil, err
	}
	if evidence["passed"] != true {
		return nil, errors.New("Python probe did not report passing assertions")
	}
	return evidence, nil
}

const descendantScript = `import os,time
pid=os.fork()
if pid==0:
 os.setsid()
 for fd in (0,1,2):
  try: os.close(fd)
  except OSError: pass
 time.sleep(1.5)
 open('LEAK_PATH','wb').write(b'escaped')
 os._exit(0)
print('ready',flush=True)
time.sleep(30)
`

func (s *suite) cancelTree(a sandbox.Observation, mode string) (map[string]any, error) {
	path := "leak-" + mode
	script := strings.ReplaceAll(descendantScript, "LEAK_PATH", path)
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	if mode == "deadline" {
		var c context.CancelFunc
		ctx, c = context.WithTimeout(s.ctx, 600*time.Millisecond)
		defer c()
	}
	ready := false
	terminals := 0
	err := s.b.Exec(ctx, a.Allocation, &guestv1.ExecRequest{Argv: []string{"/usr/bin/python3", "-u", "-c", script}, TimeoutMs: 10000}, func(e *guestv1.ExecEvent) error {
		if strings.Contains(string(e.GetStdout()), "ready") {
			ready = true
			if mode == "cancel" {
				cancel()
			}
		}
		if e.GetResult() != nil {
			terminals++
		}
		return nil
	})
	want := codes.Canceled
	if mode == "deadline" {
		want = codes.DeadlineExceeded
	}
	if !ready || status.Code(err) != want || terminals != 0 {
		return nil, fmt.Errorf("%s tree ready=%t terminals=%d status=%v", mode, ready, terminals, status.Code(err))
	}
	if err := s.noLeak(a, path); err != nil {
		return nil, err
	}
	if mode == "deadline" {
		r, e := s.exec(s.ctx, a, []string{"/bin/sleep", "5"}, 100)
		if e != nil || r.result == nil || !r.result.TimedOut {
			return nil, fmt.Errorf("command timeout must return TimedOut terminal result: %v, %s", e, r.summary())
		}
	}
	return map[string]any{"setsid_descendant_killed": true, "status": want.String(), "terminal_results": terminals}, nil
}
func (s *suite) noLeak(a sandbox.Observation, path string) error {
	r, err := s.exec(s.ctx, a, []string{"/bin/sleep", "2"}, 3000)
	if err != nil || r.result == nil || r.result.ExitCode != 0 {
		return fmt.Errorf("guest unhealthy after cancellation: %v", err)
	}
	_, err = s.b.ReadFile(s.ctx, a.Allocation, &guestv1.ReadFileRequest{Path: path})
	if status.Code(err) != codes.NotFound {
		return fmt.Errorf("escaped descendant survived or read failed: %v", err)
	}
	return nil
}
func (s *suite) disconnect(a sandbox.Observation) (map[string]any, error) {
	sess, err := s.session(a)
	if err != nil {
		return nil, err
	}
	c, err := rawClient(a.VSockPath)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+sess.Credential)
	stream, err := guestv1.NewGuestClient(c).Exec(ctx, &guestv1.ExecRequest{Argv: []string{"/usr/bin/python3", "-u", "-c", strings.ReplaceAll(descendantScript, "LEAK_PATH", "leak-disconnect")}, TimeoutMs: 10000})
	if err != nil {
		return nil, err
	}
	ready := false
	for !ready {
		e, err := stream.Recv()
		if err != nil {
			return nil, err
		}
		ready = strings.Contains(string(e.GetStdout()), "ready")
	}
	if err := c.Close(); err != nil {
		return nil, err
	}
	if err := s.noLeak(a, "leak-disconnect"); err != nil {
		return nil, err
	}
	return map[string]any{"transport_closed_after_ready": true, "setsid_descendant_killed": true}, nil
}
