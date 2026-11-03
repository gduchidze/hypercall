package sandbox

import (
	"context"
	"fmt"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	"github.com/gduchidze/hypercall/internal/snapshot"
)

// Capture is sandbox continuity at the controlled Python barrier, not a semantic
// agent checkpoint. Workspace disks and memory are fully materialized.
type CaptureRequest struct {
	Allocation         Allocation `json:"allocation"`
	CheckpointID       string     `json:"checkpoint_id"`
	ParentCheckpointID string     `json:"parent_checkpoint_id,omitempty"`
}
type RestoreRequest struct {
	OperationID  string     `json:"operation_id"`
	CheckpointID string     `json:"checkpoint_id"`
	Allocation   Allocation `json:"allocation"`
	Limits       Limits     `json:"limits"`
}

func (r RestoreRequest) Validate() error {
	if !identifier.MatchString(r.OperationID) || !identifier.MatchString(r.CheckpointID) {
		return fmt.Errorf("%w: fixed operation and checkpoint identifiers required", ErrInvalid)
	}
	return (CreateRequest{r.Allocation, r.Limits}).Validate()
}

type ForkRequest struct {
	OperationID  string          `json:"operation_id"`
	CheckpointID string          `json:"checkpoint_id"`
	Children     []CreateRequest `json:"children"`
}
type ChildResult struct {
	Allocation  Allocation  `json:"allocation"`
	Observation Observation `json:"observation"`
	Error       string      `json:"error,omitempty"`
	Interrupted bool        `json:"interrupted,omitempty"`
}
type SnapshotBackend interface {
	Capture(context.Context, CaptureRequest) (snapshot.Manifest, error)
	Restore(context.Context, RestoreRequest) (Observation, error)
	Fork(context.Context, ForkRequest) ([]ChildResult, error)
	StartControlled(context.Context, Allocation, int64) (*guestv1.ControlledState, error)
	InspectControlled(context.Context, Allocation) (*guestv1.ControlledState, error)
	ContinueControlled(context.Context, Allocation, int64) (*guestv1.ControlledState, error)
}

func (r ForkRequest) Validate() error {
	if !identifier.MatchString(r.OperationID) || !identifier.MatchString(r.CheckpointID) || len(r.Children) < 1 || len(r.Children) > 7 {
		return fmt.Errorf("%w: fixed operation/checkpoint and 1..7 children required", ErrInvalid)
	}
	seen := map[string]bool{}
	executions := map[string]bool{}
	first := r.Children[0].Allocation
	for _, child := range r.Children {
		if err := child.Validate(); err != nil {
			return err
		}
		a := child.Allocation
		if a.TenantID != first.TenantID || a.ProjectID != first.ProjectID || seen[a.SandboxID] || executions[a.ExecutionID] {
			return fmt.Errorf("%w: children require unique scoped sandbox and execution identities", ErrInvalid)
		}
		seen[a.SandboxID] = true
		executions[a.ExecutionID] = true
	}
	return nil
}
