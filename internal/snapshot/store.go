package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	Memory        = "memory"
	VMState       = "vmstate"
	Disk          = "disk"
	SchemaVersion = 1
)

type Scope struct {
	TenantID     string `json:"tenant_id"`
	ProjectID    string `json:"project_id"`
	CheckpointID string `json:"checkpoint_id"`
}

func (s Scope) prefix() string {
	return "tenants/" + s.TenantID + "/projects/" + s.ProjectID + "/checkpoints/" + s.CheckpointID
}
func (s Scope) validate() error {
	for _, v := range []string{s.TenantID, s.ProjectID, s.CheckpointID} {
		if !component.MatchString(v) {
			return ErrInvalid
		}
	}
	return nil
}

// Compatibility is intentionally conservative: every field must exactly match.
// CPUIdentity/CPUConfigSHA256 describe the effective CPU presented to the VM.
type Compatibility struct {
	Architecture           string `json:"architecture"`
	CPUIdentity            string `json:"cpu_identity"`
	CPUConfigSHA256        string `json:"cpu_config_sha256"`
	HostKernelRelease      string `json:"host_kernel_release"`
	FirecrackerVersion     string `json:"firecracker_version"`
	SnapshotFormatVersion  string `json:"snapshot_format_version"`
	TemplateSHA256         string `json:"template_sha256"`
	RootFSSHA256           string `json:"rootfs_sha256"`
	KernelSHA256           string `json:"kernel_sha256"`
	GuestSHA256            string `json:"guest_sha256"`
	DeviceLayoutVersion    string `json:"device_layout_version"`
	BarrierProtocolVersion string `json:"barrier_protocol_version"`
	BarrierConfigSHA256    string `json:"barrier_config_sha256"`
	RNGPolicy              string `json:"rng_policy"`
	VCPUs                  uint32 `json:"vcpus"`
	MemoryMiB              uint32 `json:"memory_mib"`
}

func (c Compatibility) validate() error {
	for _, v := range []string{c.CPUConfigSHA256, c.TemplateSHA256, c.RootFSSHA256, c.KernelSHA256, c.GuestSHA256, c.BarrierConfigSHA256} {
		if !digestValid(v) {
			return ErrInvalid
		}
	}
	for _, v := range []string{c.CPUIdentity, c.HostKernelRelease, c.FirecrackerVersion, c.SnapshotFormatVersion, c.DeviceLayoutVersion, c.BarrierProtocolVersion, c.RNGPolicy} {
		if v == "" || len(v) > 1024 || strings.ContainsAny(v, "\r\n\x00") {
			return ErrInvalid
		}
	}
	if (c.Architecture != "aarch64" && c.Architecture != "x86_64") || c.VCPUs < 1 || c.VCPUs > 8 || c.MemoryMiB < 128 || c.MemoryMiB > 8192 {
		return ErrInvalid
	}
	return nil
}
func digestValid(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && s == strings.ToLower(s)
}

type Source struct {
	Kind string
	Path string
}
type PublishRequest struct {
	Scope              Scope
	ParentCheckpointID string
	Compatibility      Compatibility
	Sources            []Source
}
type Artifact struct {
	Kind   string `json:"kind"`
	Key    string `json:"key"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
type Manifest struct {
	SchemaVersion      uint32        `json:"schema_version"`
	Scope              Scope         `json:"scope"`
	ParentCheckpointID string        `json:"parent_checkpoint_id,omitempty"`
	Compatibility      Compatibility `json:"compatibility"`
	UploadID           string        `json:"upload_id"`
	Artifacts          []Artifact    `json:"artifacts"`
}
type Options struct {
	Logger           *slog.Logger
	TempDir          string
	StagingTTL       time.Duration
	OperationTimeout time.Duration
}
type Store struct {
	objects Objects
	opts    Options
}

func New(objects Objects, opts Options) (*Store, error) {
	if objects == nil {
		return nil, ErrInvalid
	}
	if opts.StagingTTL == 0 {
		opts.StagingTTL = 30 * time.Minute
	}
	if opts.OperationTimeout == 0 {
		opts.OperationTimeout = 5 * time.Minute
	}
	if opts.StagingTTL < time.Millisecond || opts.OperationTimeout < time.Millisecond || opts.StagingTTL < opts.OperationTimeout || opts.StagingTTL > time.Duration(1<<63-1)-opts.OperationTimeout {
		return nil, ErrInvalid
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Store{objects: objects, opts: opts}, nil
}

type lifetime struct {
	Version        uint32    `json:"version"`
	Phase          string    `json:"phase"`
	UploadID       string    `json:"upload_id"`
	LeaseUntil     time.Time `json:"lease_until"`
	DeleteAfter    time.Time `json:"delete_after,omitempty"`
	ManifestSHA256 string    `json:"manifest_sha256,omitempty"`
	References     []string  `json:"references"`
}

func (s *Store) getJSON(ctx context.Context, key string, v any) (string, []byte, error) {
	r, g, err := s.objects.Get(ctx, key)
	if err != nil {
		return "", nil, err
	}
	defer r.Close()
	b, err := io.ReadAll(io.LimitReader(r, 1<<20+1))
	if err != nil {
		return "", nil, err
	}
	if len(b) > 1<<20 {
		return "", nil, ErrIntegrity
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(v); err != nil {
		return "", nil, fmt.Errorf("%w: invalid metadata", ErrIntegrity)
	}
	if d.Decode(new(any)) != io.EOF {
		return "", nil, ErrIntegrity
	}
	return g, b, nil
}
func (s *Store) putJSON(ctx context.Context, key string, v any, gen string) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return s.objects.Put(ctx, key, bytes.NewReader(b), int64(len(b)), gen)
}
func (s *Store) operation(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.opts.OperationTimeout)
}
func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func hashReader(ctx context.Context, r io.Reader, w io.Writer) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(h, w), &contextReader{ctx: ctx, r: r})
	return hex.EncodeToString(h.Sum(nil)), n, err
}
func (s *Store) verifyObject(ctx context.Context, a Artifact, w io.Writer) error {
	r, _, err := s.objects.Get(ctx, a.Key)
	if err != nil {
		return err
	}
	defer r.Close()
	hash, n, err := hashReader(ctx, io.LimitReader(r, a.Size+1), w)
	if err != nil {
		return err
	}
	if hash != a.SHA256 || n != a.Size {
		return ErrIntegrity
	}
	return nil
}
func (s *Store) Publish(ctx context.Context, req PublishRequest) (publishedManifest Manifest, resultErr error) {
	started := time.Now()
	defer func() {
		s.observe(ctx, "snapshot_publish", req.Scope, started, resultErr, manifestBytes(publishedManifest))
	}()
	publicationStarted := time.Now()
	ctx, cancel := s.operation(ctx)
	defer cancel()
	var zero Manifest
	if req.Scope.validate() != nil || req.Compatibility.validate() != nil || (req.ParentCheckpointID != "" && (!component.MatchString(req.ParentCheckpointID) || req.ParentCheckpointID == req.Scope.CheckpointID)) || len(req.Sources) != 3 {
		return zero, ErrInvalid
	}
	sources := append([]Source(nil), req.Sources...)
	sort.Slice(sources, func(i, j int) bool { return sources[i].Kind < sources[j].Kind })
	for i, k := range []string{Disk, Memory, VMState} {
		if sources[i].Kind != k {
			return zero, ErrInvalid
		}
	}
	m := Manifest{SchemaVersion: SchemaVersion, Scope: req.Scope, ParentCheckpointID: req.ParentCheckpointID, Compatibility: req.Compatibility, UploadID: randomID()}
	files := make([]*os.File, 0, len(sources))
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	// Compute expected content before claiming publication. No source/session
	// credentials or local paths are serialized into the manifest.
	for _, src := range sources {
		f, size, err := openSource(ctx, src.Path)
		if err != nil {
			return zero, err
		}
		files = append(files, f)
		h, n, err := hashReader(ctx, io.LimitReader(f, size+1), io.Discard)
		if err != nil {
			return zero, err
		}
		if n != size {
			return zero, ErrIntegrity
		}
		m.Artifacts = append(m.Artifacts, Artifact{Kind: src.Kind, Key: req.Scope.prefix() + "/staging/" + m.UploadID + "/" + src.Kind, SHA256: h, Size: n})
	}
	stateKey := req.Scope.prefix() + "/lifetime"
	st := lifetime{Version: 1, Phase: "publishing", UploadID: m.UploadID, LeaseUntil: publicationStarted.Add(s.opts.StagingTTL), References: []string{}}
	gen, err := s.putJSON(ctx, stateKey, st, "")
	if errors.Is(err, ErrPrecondition) {
		var existing lifetime
		if _, _, e := s.getJSON(ctx, stateKey, &existing); e != nil {
			return zero, e
		}
		if existing.Phase != "ready" {
			return zero, ErrConflict
		}
		var published Manifest
		_, b, e := s.getJSON(ctx, req.Scope.prefix()+"/manifest", &published)
		if e != nil {
			return zero, e
		}
		if sum(b) != existing.ManifestSHA256 {
			return zero, ErrIntegrity
		}
		if published.Scope != m.Scope || published.SchemaVersion != SchemaVersion || published.Compatibility != m.Compatibility || published.ParentCheckpointID != m.ParentCheckpointID || len(published.Artifacts) != len(m.Artifacts) {
			return zero, ErrConflict
		}
		for i, a := range published.Artifacts {
			want := m.Artifacts[i]
			if a.Kind != want.Kind || a.SHA256 != want.SHA256 || a.Size != want.Size {
				return zero, ErrConflict
			}
			if e = s.verifyObject(ctx, a, io.Discard); e != nil {
				return zero, e
			}
		}
		return published, nil
	}
	if err != nil {
		return zero, err
	}
	for i, a := range m.Artifacts {
		f := files[i]
		if _, e := f.Seek(0, io.SeekStart); e != nil {
			return zero, e
		}
		_, e := s.objects.Put(ctx, a.Key, f, a.Size, "")
		if e != nil {
			return zero, e
		}
		if e = s.verifyObject(ctx, a, io.Discard); e != nil {
			return zero, e
		}
	}
	b, _ := json.Marshal(m)
	if _, err = s.objects.Put(ctx, req.Scope.prefix()+"/manifest", bytes.NewReader(b), int64(len(b)), ""); err != nil {
		return zero, err
	}
	st.Phase = "ready"
	st.ManifestSHA256 = sum(b)
	if _, err = s.putJSON(ctx, stateKey, st, gen); err != nil {
		return zero, err
	}
	return m, nil
}

// Verified contains private materialized copies. Keep a reference pin until a
// restored VM no longer uses these files; Close removes only these local copies.
type Verified struct {
	Manifest Manifest
	Paths    map[string]string
	dir      string
}

func (v *Verified) Close() error { return os.RemoveAll(v.dir) }
func (s *Store) OpenVerified(ctx context.Context, scope Scope, expected Compatibility) (verified *Verified, resultErr error) {
	started := time.Now()
	defer func() {
		var size int64
		if verified != nil {
			size = manifestBytes(verified.Manifest)
		}
		s.observe(ctx, "snapshot_materialize", scope, started, resultErr, size)
	}()
	ctx, cancel := s.operation(ctx)
	defer cancel()
	if scope.validate() != nil || expected.validate() != nil {
		return nil, ErrInvalid
	}
	st, _, err := s.readLifetime(ctx, scope)
	if err != nil {
		return nil, err
	}
	if st.Phase != "ready" {
		return nil, ErrNotFound
	}
	var m Manifest
	_, b, err := s.getJSON(ctx, scope.prefix()+"/manifest", &m)
	if err != nil {
		return nil, err
	}
	if sum(b) != st.ManifestSHA256 || m.SchemaVersion != SchemaVersion || m.Scope != scope || m.Compatibility.validate() != nil || m.UploadID != st.UploadID || len(m.Artifacts) != 3 {
		return nil, ErrIntegrity
	}
	if m.Compatibility != expected {
		return nil, ErrIncompatible
	}
	dir, err := os.MkdirTemp(s.opts.TempDir, "snapshot-")
	if err != nil {
		return nil, err
	}
	v := &Verified{Manifest: m, Paths: map[string]string{}, dir: dir}
	success := false
	defer func() {
		if !success {
			v.Close()
		}
	}()
	for i, k := range []string{Disk, Memory, VMState} {
		a := m.Artifacts[i]
		if a.Kind != k || a.Key != scope.prefix()+"/staging/"+m.UploadID+"/"+k || a.Size < 1 || a.Size > maxSourceBytes || a.Size == int64(^uint64(0)>>1) || !digestValid(a.SHA256) {
			return nil, ErrIntegrity
		}
		p := filepath.Join(dir, k)
		f, e := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return nil, e
		}
		e = s.verifyObject(ctx, a, f)
		if e == nil {
			e = f.Sync()
		}
		if e == nil {
			e = f.Chmod(0400)
		}
		e = errors.Join(e, f.Close())
		if e != nil {
			return nil, e
		}
		v.Paths[k] = p
	}
	success = true
	return v, nil
}

// OpenVerifiedIn materializes under an explicit owner-private cache directory.
// The runtime can then recover only its own cache under its exclusive host lease,
// after fencing all old VMs. This does not mutate a Store shared by other hosts.
func (s *Store) OpenVerifiedIn(ctx context.Context, scope Scope, expected Compatibility, tempDir string) (*Verified, error) {
	if !filepath.IsAbs(tempDir) {
		return nil, ErrInvalid
	}
	info, err := os.Lstat(tempDir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, ErrInvalid
	}
	owned := *s
	owned.opts.TempDir = tempDir
	return owned.OpenVerified(ctx, scope, expected)
}
