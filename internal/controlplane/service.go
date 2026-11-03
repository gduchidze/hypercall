package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"
)

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func strict(raw []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return failure("invalid_argument", err.Error())
	}
	if d.Decode(new(any)) != io.EOF {
		return failure("invalid_argument", "one JSON object required")
	}
	return nil
}
func validateScope(s Scope) error {
	if s.TenantID == "" || s.ProjectID == "" {
		return failure("unauthenticated", "authenticated tenant and project required")
	}
	return nil
}
func validate(r Resource) (json.RawMessage, error) {
	if len(r.Spec) > 2<<20 {
		return nil, failure("invalid_argument", "definition exceeds 2 MiB")
	}
	if !namePattern.MatchString(r.Metadata.Name) {
		return nil, failure("invalid_argument", "name must be a lowercase resource label")
	}
	bad := func(msg string) (json.RawMessage, error) { return nil, failure("invalid_argument", msg) }
	switch r.Kind {
	case "Task":
		var s TaskSpec
		if err := strict(r.Spec, &s); err != nil {
			return nil, err
		}
		if !digestPattern.MatchString(s.TemplateDigest) || !digestPattern.MatchString(s.AgentDigest) || !digestPattern.MatchString(s.DatasetDigest) {
			return bad("task requires pinned template, agent and dataset sha256 digests")
		}
		switch s.ExecutionMode {
		case "", "foreground_v1":
			if len(s.Command) == 0 || s.ControlledCounter != 0 {
				return bad("foreground mode requires command and no controlled counter")
			}
		case "controlled_python_v1":
			if len(s.Command) != 0 || len(s.Env) != 0 || s.ControlledCounter < 0 {
				return bad("controlled Python requires empty command/env and a nonnegative counter")
			}
			s.Command = nil
			s.Env = nil
		default:
			return bad("unsupported execution mode")
		}
		if len(s.Models) != 1 {
			return bad("Phase 2A requires exactly one model; matrices are deferred")
		}
		for _, ref := range append([]Ref{s.Workspace, s.Gateway}, s.Models...) {
			if ref.ID == "" || !digestPattern.MatchString(ref.Digest) {
				return bad("references require immutable id and digest")
			}
		}
		if s.Limits.CPU < 1 || s.Limits.CPU > 8 || s.Limits.MemoryMB < 128 || s.Limits.MemoryMB > 8192 || s.Limits.DiskMB < 1 || s.Limits.DiskMB > 1024 || s.Limits.TimeoutSeconds < 1 || s.Limits.TimeoutSeconds > 3600 {
			return bad("limits require 1..8 CPU,128..8192 MiB memory,1..1024 MiB disk and 1..3600 seconds")
		}
		if len(s.Command) > 128 || len(s.Env) > 64 {
			return bad("command or environment count exceeds limit")
		}
		total := 0
		for _, arg := range s.Command {
			if strings.ContainsRune(arg, 0) {
				return bad("NUL in command")
			}
			total += len(arg)
		}
		if total > 32<<10 {
			return bad("command exceeds 32 KiB")
		}
		total = 0
		for key, value := range s.Env {
			if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(key) || strings.ContainsRune(value, 0) {
				return bad("invalid environment")
			}
			total += len(key) + len(value)
		}
		if total > 64<<10 {
			return bad("environment exceeds 64 KiB")
		}
		return encode(s), nil
	case "Workspace":
		var s WorkspaceSpec
		if err := strict(r.Spec, &s); err != nil {
			return nil, err
		}
		if !s.PrivateWrites {
			return bad("workspace requires private_writes")
		}
		if len(s.Repositories) > 16 || len(s.Files) > 128 {
			return bad("workspace input count exceeds limit")
		}
		for _, r := range s.Repositories {
			u, e := url.Parse(r.URL)
			if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || !regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`).MatchString(r.Commit) {
				return bad("repositories require HTTPS and pinned commit")
			}
		}
		paths := map[string]bool{}
		total := 0
		for _, f := range s.Files {
			if paths[f.Path] {
				return bad("duplicate workspace file path")
			}
			paths[f.Path] = true
			total += len(f.Content)
			if total > 1<<20 {
				return bad("workspace contents exceed 1 MiB")
			}
			sum := sha256.Sum256(f.Content)
			if f.Path == "" || path.IsAbs(f.Path) || path.Clean(f.Path) != f.Path || strings.HasPrefix(f.Path, "../") || f.Path == ".." || f.Digest != "sha256:"+hex.EncodeToString(sum[:]) {
				return bad("files require confined paths and matching content digests")
			}
		}
		return encode(s), nil
	case "ModelConfig":
		var s ModelConfigSpec
		if err := strict(r.Spec, &s); err != nil {
			return nil, err
		}
		if s.Provider == "" || s.Model == "" || s.Revision == "" || !digestPattern.MatchString(s.ArtifactDigest) {
			return bad("model requires provider, model, revision and artifact digest")
		}
		return encode(s), nil
	case "Gateway":
		var s GatewaySpec
		if err := strict(r.Spec, &s); err != nil {
			return nil, err
		}
		if len(s.Routes) == 0 || len(s.Routes) > 64 || len(s.SecretRefs) > 64 || len(s.AllowedHosts) > 64 {
			return bad("gateway requires routes")
		}
		for _, v := range s.Routes {
			u, e := url.Parse(v)
			if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
				return bad("routes require HTTPS without embedded credentials")
			}
		}
		for _, v := range s.SecretRefs {
			if !strings.HasPrefix(v, "secret://") {
				return bad("gateway credentials must be secret:// references")
			}
		}
		return encode(s), nil
	default:
		return bad("only Task, Workspace, ModelConfig and Gateway definitions can be created")
	}
}

type Service struct {
	store   *Store
	backend Backend
}

func NewService(store *Store, backend Backend) *Service { return &Service{store, backend} }
func (s *Service) PutResource(ctx context.Context, scope Scope, r Resource, key string) (Resource, error) {
	if e := validateScope(scope); e != nil {
		return Resource{}, e
	}
	spec, e := validate(r)
	if e != nil {
		return Resource{}, e
	}
	r.Spec = spec
	if key == "" {
		return Resource{}, failure("invalid_argument", "idempotency key required")
	}
	if s.store == nil {
		return Resource{}, failure("unavailable", "database unavailable")
	}
	return s.store.PutResource(ctx, scope, r, key)
}
func (s *Service) GetResource(ctx context.Context, scope Scope, kind, id string) (Resource, error) {
	if e := validateScope(scope); e != nil {
		return Resource{}, e
	}
	return s.store.GetResource(ctx, scope, kind, id)
}
func (s *Service) ListResources(ctx context.Context, scope Scope, kind, after string, limit int) ([]Resource, error) {
	if e := validateScope(scope); e != nil {
		return nil, e
	}
	return s.store.ListResources(ctx, scope, kind, after, limit)
}
func (s *Service) Submit(ctx context.Context, scope Scope, taskID, key string) (Submission, error) {
	if e := validateScope(scope); e != nil {
		return Submission{}, e
	}
	return s.store.Submit(ctx, scope, taskID, key)
}
func (s *Service) Action(ctx context.Context, scope Scope, branchID, action, key string) (Resource, error) {
	if e := validateScope(scope); e != nil {
		return Resource{}, e
	}
	admit := func(run RunSpec) error {
		if action == "suspend" || action == "resume" {
			if capable, ok := s.backend.(interface{ SupportsRun(string, RunSpec) bool }); ok {
				if !capable.SupportsRun(action, run) {
					return failure("failed_precondition", "sandbox backend does not support "+action+" for this resolved task")
				}
			} else if capable, ok := s.backend.(interface{ Supports(string) bool }); !ok || !capable.Supports(action) {
				return failure("failed_precondition", "sandbox backend does not support "+action)
			}
		}
		return nil
	}
	if s.store == nil {
		if e := admit(RunSpec{}); e != nil {
			return Resource{}, e
		}
		return Resource{}, failure("unavailable", "database unavailable")
	}
	return s.store.action(ctx, scope, branchID, action, key, admit)
}
func (s *Service) Events(ctx context.Context, scope Scope, branchID string, after int64, limit int) ([]Event, error) {
	if e := validateScope(scope); e != nil {
		return nil, e
	}
	return s.store.Events(ctx, scope, branchID, after, limit)
}

func validateNativeCheckpoint(cp CheckpointSpec) error {
	if cp.NativeCheckpointID == "" && len(cp.SnapshotManifest) == 0 {
		return nil
	}
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`).MatchString(cp.NativeCheckpointID) || len(cp.SnapshotManifest) == 0 || len(cp.SnapshotManifest) > 256<<10 || !json.Valid(cp.SnapshotManifest) || cp.SnapshotManifest[0] != '{' {
		return failure("failed_precondition", "native checkpoint requires bounded manifest JSON and identity")
	}
	sum := sha256.Sum256(cp.SnapshotManifest)
	if cp.ManifestDigest != "sha256:"+hex.EncodeToString(sum[:]) {
		return failure("failed_precondition", "native snapshot manifest digest mismatch")
	}
	return nil
}
