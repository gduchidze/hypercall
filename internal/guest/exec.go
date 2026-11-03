package guest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func validPath(path string) bool {
	if path == "" || len(path) > 4096 || strings.ContainsRune(path, 0) || filepath.IsAbs(path) {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

func (s *Server) validateExec(req *guestv1.ExecRequest) error {
	if len(req.Argv) == 0 || len(req.Argv) > 128 || req.Argv[0] == "" || req.TimeoutMs == 0 || time.Duration(req.TimeoutMs)*time.Millisecond > MaxExecTimeout {
		return status.Error(codes.InvalidArgument, "argv and bounded positive timeout are required")
	}
	bytes := 0
	for _, arg := range req.Argv {
		bytes += len(arg)
		if strings.ContainsRune(arg, 0) {
			return status.Error(codes.InvalidArgument, "invalid argv")
		}
	}
	if bytes > 32768 || (req.Cwd != "" && !validPath(req.Cwd)) {
		return status.Error(codes.InvalidArgument, "invalid argv or workspace cwd")
	}
	if len(req.Env) > 64 {
		return status.Error(codes.InvalidArgument, "environment exceeds limit")
	}
	for key, value := range req.Env {
		bytes += len(key) + len(value)
		if !s.allowedEnv[key] || !envName.MatchString(key) || strings.ContainsRune(value, 0) || len(value) > 4096 {
			return status.Error(codes.InvalidArgument, "environment key not allowed or value invalid")
		}
	}
	if bytes > 65536 {
		return status.Error(codes.InvalidArgument, "argv and environment exceed limit")
	}
	return nil
}

// Exec is foreground-only. Production commands enter a private cgroup before
// executing; cancellation and completion kill all descendants, including those
// that create a new session. Trusted local tests use process groups only.
func (s *Server) Exec(req *guestv1.ExecRequest, stream grpc.ServerStreamingServer[guestv1.ExecEvent]) (resultErr error) {
	s.opMu.RLock()
	defer s.opMu.RUnlock()
	if err := s.ordinary(); err != nil {
		return err
	}
	if err := s.authenticate(stream.Context()); err != nil {
		return err
	}
	if err := s.validateExec(req); err != nil {
		return err
	}
	select {
	case s.execSlots <- struct{}{}:
		defer func() { <-s.execSlots }()
	default:
		return status.Error(codes.ResourceExhausted, "foreground execution concurrency limit reached")
	}
	cwd := req.Cwd
	if cwd == "" {
		cwd = "."
	}
	dir, err := s.root.OpenFile(cwd, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fileError(err)
	}
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil || !info.IsDir() {
		return status.Error(codes.InvalidArgument, "cwd must be a workspace directory")
	}
	deadline := time.Now().Add(time.Duration(req.TimeoutMs) * time.Millisecond)
	if s.expiresAt.Before(deadline) {
		deadline = s.expiresAt
	}
	ctx, cancel := context.WithDeadline(stream.Context(), deadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.execHelper, append([]string{"--hypercall-internal-exec"}, req.Argv...)...)
	// The helper performs fchdir on the pinned workspace descriptor after uid/gid
	// dropping, then replaces itself. No path re-resolution or /proc permission
	// assumptions occur between validating cwd and using it.
	cmd.ExtraFiles = []*os.File{dir}
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8"}
	keys := make([]string, 0, len(req.Env))
	for key := range req.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		cmd.Env = append(cmd.Env, key+"="+req.Env[key])
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if !s.trustedLocal {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(s.uid), Gid: uint32(s.gid), Groups: []uint32{uint32(s.gid)}}
	}
	group, err := commandCgroup(s.workloadCgroup, cmd)
	if err != nil {
		return status.Error(codes.FailedPrecondition, "workload containment unavailable")
	}
	defer func() {
		if err := group.close(); err != nil {
			s.logger.Error("workload containment cleanup failed", "error", err)
			if resultErr == nil {
				resultErr = status.Error(codes.Internal, "workload containment cleanup failed")
			}
		}
	}()
	cmd.Cancel = func() error {
		if err := group.kill(); err != nil {
			s.logger.Error("workload cgroup cancellation failed", "error", err)
		}
		return killGroup(cmd)
	}
	cmd.WaitDelay = time.Second
	events := make(chan *guestv1.ExecEvent, 8)
	output := &outputBudget{ctx: ctx, cancel: cancel, events: events}
	cmd.Stdout = &outputWriter{budget: output}
	cmd.Stderr = &outputWriter{budget: output, stderr: true}
	started := time.Now()
	if err := cmd.Start(); err != nil {
		return status.Error(codes.FailedPrecondition, "command could not start")
	}
	waited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		if killErr := group.kill(); killErr != nil {
			err = killErr
		}
		// Foreground completion also ends children which retained the group.
		_ = killGroup(cmd)
		waited <- err
		close(events)
	}()
	var sendErr error
	for event := range events {
		if sendErr == nil {
			sendErr = sendEvent(stream, event)
			if sendErr != nil {
				cancel()
			}
		}
	}
	waitErr := <-waited
	if sendErr != nil {
		return sendErr
	}
	if err := stream.Context().Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	result := &guestv1.ExecResult{ExitCode: int32(cmd.ProcessState.ExitCode()), TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded), OutputTruncated: output.truncated}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) && ctx.Err() == nil {
		return status.Error(codes.Internal, "command output failed")
	}
	s.logger.InfoContext(stream.Context(), "guest command completed", "duration_ms", time.Since(started).Milliseconds(), "exit_code", result.ExitCode, "timed_out", result.TimedOut, "output_truncated", result.OutputTruncated, "output_bytes", output.used)
	return sendEvent(stream, &guestv1.ExecEvent{Event: &guestv1.ExecEvent_Result{Result: result}})
}

func executablePath(name string) (string, error) {
	if strings.ContainsRune(name, '/') {
		return name, nil
	}
	for _, dir := range []string{"/usr/local/bin", "/usr/bin", "/bin"} {
		if path, err := exec.LookPath(filepath.Join(dir, name)); err == nil {
			return path, nil
		}
	}
	return "", exec.ErrNotFound
}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

// A stalled receiver must not keep the foreground process alive. At most one
// Send is in flight; returning from the handler closes its transport stream and
// releases a blocked Send. The goroutine owns its event until Send completes.
func sendEvent(stream grpc.ServerStreamingServer[guestv1.ExecEvent], event *guestv1.ExecEvent) error {
	done := make(chan error, 1)
	go func() { done <- stream.Send(event) }()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-stream.Context().Done():
		return status.FromContextError(stream.Context().Err()).Err()
	case <-timer.C:
		return status.Error(codes.ResourceExhausted, "output receiver stalled")
	}
}

type outputBudget struct {
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	events    chan<- *guestv1.ExecEvent
	used      int
	truncated bool
}

type outputWriter struct {
	budget *outputBudget
	stderr bool
}

func (w *outputWriter) Write(p []byte) (int, error) {
	n := len(p)
	b := w.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.truncated {
		return n, nil
	}
	remaining := MaxExecOutputBytes - b.used
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	for len(p) > 0 {
		size := min(len(p), 16<<10)
		chunk := append([]byte(nil), p[:size]...)
		p = p[size:]
		event := &guestv1.ExecEvent{Event: &guestv1.ExecEvent_Stdout{Stdout: chunk}}
		if w.stderr {
			event.Event = &guestv1.ExecEvent_Stderr{Stderr: chunk}
		}
		select {
		case b.events <- event:
			b.used += size
		case <-b.ctx.Done():
			return n, nil
		}
	}
	if b.truncated {
		b.cancel()
	}
	return n, nil
}

// RunExecHelper handles the explicit internal child mode in the guest executable.
// Fchdir pins the directory without a path validation/use race. It returns in
// ordinary server mode, and never returns in helper mode.
func RunExecHelper() {
	if len(os.Args) < 2 || os.Args[1] != "--hypercall-internal-exec" {
		return
	}
	if len(os.Args) < 3 || syscall.Fchdir(3) != nil {
		os.Exit(126)
	}
	_ = syscall.Close(3)
	if err := preventPrivilegeGain(); err != nil {
		os.Exit(126)
	}
	path, err := executablePath(os.Args[2])
	if err != nil {
		os.Exit(127)
	}
	if syscall.Exec(path, os.Args[2:], os.Environ()) != nil {
		os.Exit(126)
	}
}
