package controlplane

import (
	"context"
	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	"time"
)

// Assignment is an expiring, fenced execution capability. Backend implementations
// MUST reject expired/lower epochs and self-fence at LeaseUntil without the API.
// Repeating Ensure or Resume with the same execution/epoch/checkpoint must converge
// without duplicate execution. Suspend must locally fence the predecessor before
// reporting a verified checkpoint. Resume must renew guest identity, credentials
// and transports before continuation; it cannot fall back to cold Ensure.
type Assignment struct {
	Scope                            Scope
	BranchID, SandboxID, ExecutionID string
	Epoch                            int64
	LeaseUntil                       time.Time
	Run                              RunSpec
}
type Observation struct {
	State      string
	Checkpoint *CheckpointSpec
}
type CheckpointSpec struct {
	NativeCheckpointID string            `json:"native_checkpoint_id,omitempty"`
	SnapshotManifest   []byte            `json:"snapshot_manifest,omitempty"`
	BranchID           string            `json:"branch_id"`
	Compatibility      map[string]string `json:"compatibility"`
	ManifestDigest     string            `json:"manifest_digest"`
	ManifestURI        string            `json:"manifest_uri"`
	ProtocolVersion    string            `json:"protocol_version"`
	// EventPosition zero omits the cursor; a nonzero cursor must equal the
	// controller committed position at successful suspension.
	EventPosition int64 `json:"event_position"`
	Verified      bool  `json:"verified"`
}
type Backend interface {
	Ensure(context.Context, Assignment) (Observation, error)
	Observe(context.Context, Assignment) (Observation, error)
	Stop(context.Context, Assignment) (Observation, error)
	Suspend(context.Context, Assignment) (Observation, error)
	Resume(context.Context, Assignment, CheckpointSpec) (Observation, error)
	Exec(context.Context, Assignment, *guestv1.ExecRequest, func(*guestv1.ExecEvent) error) error
	ReadFile(context.Context, Assignment, *guestv1.ReadFileRequest) (*guestv1.ReadFileResponse, error)
	WriteFile(context.Context, Assignment, *guestv1.WriteFileRequest) (*guestv1.WriteFileResponse, error)
}

// UnavailableBackend is deliberate: no Phase 1 runtime is implicitly
// selected by default. It cannot report a VM as running or a checkpoint as verified.
type UnavailableBackend struct{}

func (UnavailableBackend) Supports(action string) bool { return false }
func (UnavailableBackend) Ensure(context.Context, Assignment) (Observation, error) {
	return Observation{}, failure("unavailable", "Phase 1 sandbox backend is unavailable")
}
func (b UnavailableBackend) Observe(c context.Context, a Assignment) (Observation, error) {
	return b.Ensure(c, a)
}
func (b UnavailableBackend) Stop(c context.Context, a Assignment) (Observation, error) {
	return b.Ensure(c, a)
}
func (b UnavailableBackend) Suspend(c context.Context, a Assignment) (Observation, error) {
	return b.Ensure(c, a)
}
func (b UnavailableBackend) Resume(c context.Context, a Assignment, _ CheckpointSpec) (Observation, error) {
	return b.Ensure(c, a)
}
func (UnavailableBackend) Exec(context.Context, Assignment, *guestv1.ExecRequest, func(*guestv1.ExecEvent) error) error {
	return failure("unavailable", "sandbox backend unavailable")
}
func (UnavailableBackend) ReadFile(context.Context, Assignment, *guestv1.ReadFileRequest) (*guestv1.ReadFileResponse, error) {
	return nil, failure("unavailable", "sandbox backend unavailable")
}
func (UnavailableBackend) WriteFile(context.Context, Assignment, *guestv1.WriteFileRequest) (*guestv1.WriteFileResponse, error) {
	return nil, failure("unavailable", "sandbox backend unavailable")
}
