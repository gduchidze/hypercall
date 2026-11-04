package guest

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type agentProcess struct {
	cmd      *exec.Cmd
	done     chan struct{}
	stopOnce sync.Once
	kill     func()
}

func (a *agentProcess) stop() { a.stopOnce.Do(a.kill); <-a.done }
func (s *Server) agentHealthyLocked() error {
	if s.agent == nil {
		return nil
	}
	select {
	case <-s.agent.done:
		s.harness.mu.Lock()
		complete := s.harness.finalized
		s.harness.mu.Unlock()
		if !complete {
			return status.Error(codes.Unavailable, "cooperative process exited without committed finalization")
		}
	default:
	}
	return nil
}
func (s *Server) StartAgent(ctx context.Context, _ *guestv1.StartAgentRequest) (*guestv1.GetInfoResponse, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if e := s.authenticate(ctx); e != nil {
		return nil, e
	}
	if e := s.ordinary(); e != nil {
		return nil, e
	}
	if s.harness == nil || !filepath.IsAbs(s.harnessSocket) || !filepath.IsAbs(s.harnessProgram) {
		return nil, status.Error(codes.FailedPrecondition, "verified cooperative program and local socket required")
	}
	if s.controlled != nil || len(s.execSlots) != 0 {
		return nil, status.Error(codes.FailedPrecondition, "another guest workload exists")
	}
	if s.agent != nil {
		select {
		case <-s.agent.done:
			return nil, status.Error(codes.Unavailable, "cooperative agent exited")
		default:
			return s.infoLocked(), nil
		}
	}
	dir, e := s.root.Open(".")
	if e != nil {
		return nil, fileError(e)
	}
	defer dir.Close()
	cmd := exec.Command(s.execHelper, "--hypercall-internal-exec", "/usr/bin/python3", "-I", "-u", s.harnessProgram, "--socket", s.harnessSocket)
	cmd.ExtraFiles = []*os.File{dir}
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if !s.trustedLocal {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(s.uid), Gid: uint32(s.gid), Groups: []uint32{uint32(s.gid)}}
	}
	group, e := commandCgroup(s.workloadCgroup, cmd)
	if e != nil {
		return nil, status.Error(codes.FailedPrecondition, "cooperative workload containment unavailable")
	}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if e = cmd.Start(); e != nil {
		group.close()
		return nil, status.Error(codes.FailedPrecondition, "verified cooperative Python unavailable")
	}
	a := &agentProcess{cmd: cmd, done: make(chan struct{}), kill: func() { _ = group.kill(); _ = killGroup(cmd) }}
	s.agent = a
	go func() { _ = cmd.Wait(); a.stopOnce.Do(a.kill); _ = group.close(); close(a.done) }()
	return s.infoLocked(), nil
}
