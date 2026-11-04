// Package guest implements the private authenticated foreground execution seam.
// It must run inside a VM. TrustedLocal exists only for tests; os/exec is not a
// host sandbox. VM/cgroup qualification is separate from local RPC conformance.
package guest

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	harnessv1 "github.com/gduchidze/hypercall/gen/go/hypercall/harness/v1"
	"github.com/gduchidze/hypercall/internal/harness"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	MaxFileBytes       = 1 << 20
	MaxExecOutputBytes = 1 << 20
	MaxExecTimeout     = 10 * time.Minute
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

type Config struct {
	HarnessSession *harnessv1.HarnessSession
	HarnessSocket  string
	HarnessProgram string
	Workspace      string
	Identity       *guestv1.AllocationIdentity
	Credential     string
	ExpiresAt      time.Time
	// TrustedLocal permits same-user execution ONLY in explicit local tests.
	TrustedLocal bool
	// ExecHelper is this guest executable (or a test executable whose
	// TestMain calls RunExecHelper), used to fchdir before exec.
	ExecHelper string
	UID, GID   int
	AllowedEnv []string
	// WorkloadCgroup is an empty, delegated cgroup v2 subtree with finite
	// memory, CPU and PID limits. Required for production execution.
	WorkloadCgroup string
	Logger         *slog.Logger
}

type Server struct {
	harnessv1.UnimplementedHarnessServer
	harness                       *harnessState
	agent                         *agentProcess
	harnessSocket, harnessProgram string
	opMu                          sync.RWMutex
	captureGate                   bool
	controlled                    *controlledProcess
	guestv1.UnimplementedGuestServer
	root           *os.Root
	identity       *guestv1.AllocationIdentity
	credentialHash [32]byte
	expiresAt      time.Time
	trustedLocal   bool
	execHelper     string
	uid, gid       int
	allowedEnv     map[string]bool
	logger         *slog.Logger
	execSlots      chan struct{}
	workloadCgroup string
}

func New(cfg Config) (*Server, error) {
	if cfg.Identity == nil || cfg.Identity.GetEpoch() == 0 {
		return nil, errors.New("nonzero allocation identity required")
	}
	for _, value := range []string{cfg.Identity.TenantId, cfg.Identity.ProjectId, cfg.Identity.SandboxId, cfg.Identity.ExecutionId} {
		if !identifier.MatchString(value) {
			return nil, errors.New("invalid allocation identity")
		}
	}
	if len(cfg.Credential) < 32 || len(cfg.Credential) > 512 || strings.ContainsAny(cfg.Credential, " \t\r\n") {
		return nil, errors.New("credential must be 32..512 non-whitespace bytes")
	}
	if !cfg.ExpiresAt.After(time.Now()) || time.Until(cfg.ExpiresAt) > 24*time.Hour {
		return nil, errors.New("session expiry must be future and within 24 hours")
	}
	if !cfg.TrustedLocal && (runtime.GOOS != "linux" || os.Geteuid() != 0 || cfg.UID <= 0 || cfg.GID <= 0 || uint64(cfg.UID) > 1<<32-1 || uint64(cfg.GID) > 1<<32-1) {
		return nil, errors.New("production execution requires a Linux root daemon and non-root workload uid/gid")
	}
	if !filepath.IsAbs(cfg.ExecHelper) {
		return nil, errors.New("absolute guest helper executable required")
	}
	if err := validateWorkloadCgroup(cfg.WorkloadCgroup, cfg.TrustedLocal); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(cfg.Workspace) {
		return nil, errors.New("absolute workspace required")
	}
	allowed := make(map[string]bool, len(cfg.AllowedEnv))
	for _, key := range cfg.AllowedEnv {
		if !envName.MatchString(key) {
			return nil, errors.New("invalid environment allowlist")
		}
		allowed[key] = true
	}
	if cfg.HarnessSession != nil {
		if e := harness.ValidateSession(cfg.HarnessSession); e != nil {
			return nil, e
		}
		h := cfg.HarnessSession
		if h.TenantId != cfg.Identity.TenantId || h.ProjectId != cfg.Identity.ProjectId || h.SandboxId != cfg.Identity.SandboxId || h.ExecutionId != cfg.Identity.ExecutionId || h.Epoch != int64(cfg.Identity.Epoch) {
			return nil, errors.New("harness session allocation mismatch")
		}
	}
	root, err := os.OpenRoot(cfg.Workspace)
	if err != nil {
		return nil, errors.New("open workspace failed")
	}
	if !cfg.TrustedLocal {
		info, statErr := root.Stat(".")
		if statErr != nil {
			_ = root.Close()
			return nil, errors.New("stat workspace failed")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(cfg.UID) || stat.Gid != uint32(cfg.GID) || info.Mode().Perm()&0o700 != 0o700 {
			_ = root.Close()
			return nil, errors.New("workspace must be owned and accessible by workload uid/gid")
		}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{harnessSocket: cfg.HarnessSocket, harnessProgram: cfg.HarnessProgram, harness: newHarnessState(cfg.HarnessSession), root: root, identity: proto.Clone(cfg.Identity).(*guestv1.AllocationIdentity), credentialHash: sha256.Sum256([]byte(cfg.Credential)), expiresAt: cfg.ExpiresAt, trustedLocal: cfg.TrustedLocal, execHelper: cfg.ExecHelper, uid: cfg.UID, gid: cfg.GID, allowedEnv: allowed, logger: logger, execSlots: make(chan struct{}, 16), workloadCgroup: cfg.WorkloadCgroup}, nil
}

// Close is called after all RPCs are stopped.
func (s *Server) Close() error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.controlled != nil {
		_ = s.controlled.cleanup()
	}
	if s.agent != nil {
		s.agent.stop()
	}
	return s.root.Close()
}

func (s *Server) authenticate(ctx context.Context) error {
	values := metadata.ValueFromIncomingContext(ctx, "authorization")
	valid := false
	if len(values) == 1 && strings.HasPrefix(values[0], "Bearer ") && len(values[0]) <= 519 {
		candidate := sha256.Sum256([]byte(strings.TrimPrefix(values[0], "Bearer ")))
		valid = subtle.ConstantTimeCompare(candidate[:], s.credentialHash[:]) == 1
	}
	if !valid || !time.Now().Before(s.expiresAt) {
		s.logger.WarnContext(ctx, "guest authentication rejected")
		return status.Error(codes.Unauthenticated, "invalid or expired allocation session")
	}
	return status.FromContextError(ctx.Err()).Err()
}

func (s *Server) GetInfo(ctx context.Context, _ *guestv1.GetInfoRequest) (*guestv1.GetInfoResponse, error) {
	s.opMu.RLock()
	defer s.opMu.RUnlock()
	if err := s.authenticate(ctx); err != nil {
		return nil, err
	}
	if s.captureGate {
		return nil, status.Error(codes.Unauthenticated, "session renewal required at capture gate")
	}
	if err := s.agentHealthyLocked(); err != nil {
		return nil, err
	}
	return s.infoLocked(), nil
}
func (s *Server) infoLocked() *guestv1.GetInfoResponse {
	info := &guestv1.GetInfoResponse{ProtocolVersion: "hypercall.guest.v1", Identity: proto.Clone(s.identity).(*guestv1.AllocationIdentity), MaxFileBytes: MaxFileBytes, MaxExecOutputBytes: MaxExecOutputBytes, ServerTimeUnixNano: time.Now().UnixNano()}
	if s.harness != nil {
		s.harness.mu.Lock()
		info.HarnessSessionHash = s.harness.session.SessionHash
		s.harness.mu.Unlock()
		info.AgentStarted = s.agent != nil
	}
	return info
}
