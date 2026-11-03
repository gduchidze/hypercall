// Package sandbox owns the host-local cold-boot lifecycle. It is an adapter for
// the controller, not a scheduler or a public control plane. Scope is supplied
// by a trusted, authenticated host caller; request identity is not authentication.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
)

var (
	ErrInvalid     = errors.New("invalid sandbox request")
	ErrNotFound    = errors.New("allocation not found")
	ErrConflict    = errors.New("allocation already exists or epoch is stale")
	ErrClosed      = errors.New("sandbox backend closed")
	ErrUnavailable = errors.New("sandbox unavailable")
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

type Allocation struct {
	TenantID    string `json:"tenant_id"`
	ProjectID   string `json:"project_id"`
	SandboxID   string `json:"sandbox_id"`
	ExecutionID string `json:"execution_id"`
	Epoch       uint64 `json:"epoch"`
}

func (a Allocation) Validate() error {
	for _, s := range []string{a.TenantID, a.ProjectID, a.SandboxID, a.ExecutionID} {
		if !identifier.MatchString(s) {
			return fmt.Errorf("%w: allocation identifiers", ErrInvalid)
		}
	}
	if a.Epoch == 0 {
		return fmt.Errorf("%w: epoch must be positive", ErrInvalid)
	}
	return nil
}
func (a Allocation) identity() *guestv1.AllocationIdentity {
	return &guestv1.AllocationIdentity{TenantId: a.TenantID, ProjectId: a.ProjectID, SandboxId: a.SandboxID, ExecutionId: a.ExecutionID, Epoch: a.Epoch}
}

type Limits struct {
	VCPUs          uint32        `json:"vcpus"`
	MemoryMiB      uint32        `json:"memory_mib"`
	WorkspaceBytes int64         `json:"workspace_bytes"`
	PIDs           uint32        `json:"pids"`
	TTL            time.Duration `json:"ttl"`
}

// An admitted Create consumes its host-local sandbox epoch even if boot fails.
// A retry needs a strictly greater epoch; fencing persists across host restarts.
type CreateRequest struct {
	Allocation Allocation `json:"allocation"`
	Limits     Limits     `json:"limits"`
}

func (r CreateRequest) Validate() error {
	if err := r.Allocation.Validate(); err != nil {
		return err
	}
	l := r.Limits
	if l.VCPUs < 1 || l.VCPUs > 8 || l.MemoryMiB < 128 || l.MemoryMiB > 8192 || l.WorkspaceBytes < 1<<20 || l.WorkspaceBytes > 1<<30 || l.WorkspaceBytes > int64(l.MemoryMiB-64)<<20 || l.PIDs < 8 || l.PIDs > 512 || l.TTL < time.Second || l.TTL > time.Hour {
		return fmt.Errorf("%w: limits outside supported envelope (1..8 CPUs, 128..8192 MiB, 1 MiB..1 GiB workspace within guest memory, 8..512 PIDs, 1s..1h TTL)", ErrInvalid)
	}
	return nil
}

type State string

const (
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateStopped  State = "stopped"
	StateFailed   State = "failed"
)

type Observation struct {
	Allocation Allocation `json:"allocation"`
	State      State      `json:"state"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	PID        int        `json:"pid,omitempty"`
	CgroupPath string     `json:"cgroup_path,omitempty"`
	RootDir    string     `json:"root_dir,omitempty"`
	VSockPath  string     `json:"vsock_path,omitempty"`
}

// Config is copied by New. ArtifactConfigPath uses the hostdoctor schema and
// pinned official Firecracker/jailer 1.14.0. StateDir must be private and durable
// across host-process restarts; CgroupDir is a delegated cgroups-v2 parent.
// mkfs.ext4 is required for a bounded, read-only per-allocation bootstrap disk.
type Scope struct {
	TenantID  string
	ProjectID string
}

type Config struct {
	Scope              Scope
	ArtifactConfigPath string
	StateDir           string
	CgroupDir          string
	JailerUID          int
	JailerGID          int
	Logger             *slog.Logger
}

type SandboxBackend interface {
	Create(context.Context, CreateRequest) (Observation, error)
	Inspect(context.Context, Allocation) (Observation, error)
	Exec(context.Context, Allocation, *guestv1.ExecRequest, func(*guestv1.ExecEvent) error) error
	ReadFile(context.Context, Allocation, *guestv1.ReadFileRequest) (*guestv1.ReadFileResponse, error)
	WriteFile(context.Context, Allocation, *guestv1.WriteFileRequest) (*guestv1.WriteFileResponse, error)
	Stop(context.Context, Allocation) error
	Recover(context.Context) ([]Observation, error)
	Close() error
}

func validateConfig(c Config) error {
	if !identifier.MatchString(c.Scope.TenantID) || !identifier.MatchString(c.Scope.ProjectID) {
		return fmt.Errorf("%w: trusted tenant/project scope required", ErrInvalid)
	}
	if c.ArtifactConfigPath == "" || c.StateDir == "" || c.CgroupDir == "" || c.JailerUID <= 0 || c.JailerGID <= 0 || uint64(c.JailerUID) > 1<<32-1 || uint64(c.JailerGID) > 1<<32-1 {
		return fmt.Errorf("%w: explicit artifacts, private state, delegated cgroup and non-root jailer uid/gid required", ErrInvalid)
	}
	return nil
}

var _ SandboxBackend = (*Backend)(nil)
