// Package controlplane owns durable intent and the single sandbox lifecycle controller.
package controlplane

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type Scope struct {
	TenantID  string `json:"tenant_id"`
	ProjectID string `json:"project_id"`
}
type Metadata struct {
	ID            string    `json:"id"`
	TenantID      string    `json:"tenant_id"`
	ProjectID     string    `json:"project_id"`
	Name          string    `json:"name"`
	SchemaVersion string    `json:"schema_version"`
	Revision      int64     `json:"revision"`
	Digest        string    `json:"digest"`
	CreatedAt     time.Time `json:"created_at"`
}
type Resource struct {
	Metadata Metadata        `json:"metadata"`
	Kind     string          `json:"kind"`
	Spec     json.RawMessage `json:"spec"`
	Status   json.RawMessage `json:"status,omitempty"`
}
type Ref struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
}
type Limits struct {
	CPU            int32 `json:"cpu"`
	MemoryMB       int32 `json:"memory_mb"`
	DiskMB         int32 `json:"disk_mb"`
	TimeoutSeconds int32 `json:"timeout_seconds"`
}
type TaskSpec struct {
	ExecutionMode     string            `json:"execution_mode,omitempty"`
	ControlledCounter int64             `json:"controlled_counter,omitempty"`
	Workspace         Ref               `json:"workspace"`
	Models            []Ref             `json:"models"`
	Gateway           Ref               `json:"gateway"`
	TemplateDigest    string            `json:"template_digest"`
	AgentDigest       string            `json:"agent_digest"`
	DatasetDigest     string            `json:"dataset_digest"`
	Command           []string          `json:"command"`
	Env               map[string]string `json:"env,omitempty"`
	Limits            Limits            `json:"limits"`
}
type Repository struct {
	URL    string `json:"url"`
	Commit string `json:"commit"`
}
type InputFile struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
	Digest  string `json:"digest"`
}
type WorkspaceSpec struct {
	Repositories  []Repository `json:"repositories,omitempty"`
	Files         []InputFile  `json:"files,omitempty"`
	PrivateWrites bool         `json:"private_writes"`
}
type ModelConfigSpec struct {
	Provider       string            `json:"provider"`
	Model          string            `json:"model"`
	Revision       string            `json:"revision"`
	ArtifactDigest string            `json:"artifact_digest"`
	Parameters     map[string]string `json:"parameters,omitempty"`
}
type GatewaySpec struct {
	Routes       map[string]string `json:"routes"`
	SecretRefs   []string          `json:"secret_refs,omitempty"`
	AllowedHosts []string          `json:"allowed_hosts,omitempty"`
}
type RunSpec struct {
	Task      Resource   `json:"task"`
	Workspace Resource   `json:"workspace"`
	Models    []Resource `json:"models"`
	Gateway   Resource   `json:"gateway"`
}
type BranchSpec struct {
	ParentBranchID     string   `json:"parent_branch_id,omitempty"`
	ForkOperationID    string   `json:"fork_operation_id,omitempty"`
	RunID              string   `json:"run_id"`
	Model              Resource `json:"model"`
	ParentCheckpointID string   `json:"parent_checkpoint_id,omitempty"`
}
type BranchStatus struct {
	AgentGate         string    `json:"agent_gate,omitempty"`
	RouteDigest       string    `json:"route_digest,omitempty"`
	AgentStep         int64     `json:"agent_step"`
	TraceComplete     bool      `json:"trace_complete"`
	SandboxID         string    `json:"sandbox_id"`
	CheckpointID      string    `json:"checkpoint_id,omitempty"`
	BudgetRemainingMS int64     `json:"budget_remaining_ms"`
	Desired           string    `json:"desired"`
	Observed          string    `json:"observed"`
	ExecutionID       string    `json:"execution_id"`
	Epoch             int64     `json:"epoch"`
	LeaseUntil        time.Time `json:"lease_until"`
	LastEvent         int64     `json:"last_event"`
	Error             string    `json:"error,omitempty"`
}
type OperationStatus struct {
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}
type OperationSpec struct {
	ChildBranchIDs    []string `json:"child_branch_ids,omitempty"`
	ParentOperationID string   `json:"parent_operation_id,omitempty"`
	CheckpointID      string   `json:"checkpoint_id,omitempty"`
	BranchID          string   `json:"branch_id"`
	Action            string   `json:"action"`
}
type Submission struct {
	Run       Resource `json:"run"`
	Branch    Resource `json:"branch"`
	Operation Resource `json:"operation"`
}
type Event struct {
	ID        string          `json:"id"`
	BranchID  string          `json:"branch_id"`
	Sequence  int64           `json:"sequence"`
	Epoch     int64           `json:"epoch"`
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data"`
	CreatedAt time.Time       `json:"created_at"`
}
type Error struct {
	Kind    string
	Message string
}

func (e *Error) Error() string { return e.Message }
func Code(err error) string {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return "internal"
}
func failure(code, msg string) error { return &Error{code, msg} }
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func encode(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("encode: %v", err))
	}
	return b
}
