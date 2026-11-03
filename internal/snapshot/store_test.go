package snapshot_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gduchidze/hypercall/internal/snapshot"
	"golang.org/x/sys/unix"
)

func compatibility() snapshot.Compatibility {
	return snapshot.Compatibility{Architecture: "aarch64", CPUIdentity: "test-cpu", CPUConfigSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", HostKernelRelease: "6.8.0-test", FirecrackerVersion: "1.14.0", SnapshotFormatVersion: "1.14.0", TemplateSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RootFSSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", KernelSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", GuestSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", DeviceLayoutVersion: "v1", BarrierProtocolVersion: "v1", BarrierConfigSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RNGPolicy: "reseed-before-resume", VCPUs: 1, MemoryMiB: 128}
}
func scope() snapshot.Scope {
	return snapshot.Scope{TenantID: "tenant", ProjectID: "project", CheckpointID: "checkpoint"}
}
func newStore(t *testing.T) (*snapshot.Store, *snapshot.FileObjects) {
	t.Helper()
	objects, err := snapshot.NewFileObjects(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := snapshot.New(objects, snapshot.Options{TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return store, objects
}
func request(t *testing.T) snapshot.PublishRequest {
	t.Helper()
	var sources []snapshot.Source
	for _, kind := range []string{snapshot.Memory, snapshot.VMState, snapshot.Disk} {
		p := filepath.Join(t.TempDir(), kind)
		if err := os.WriteFile(p, []byte("abc"), 0600); err != nil {
			t.Fatal(err)
		}
		sources = append(sources, snapshot.Source{Kind: kind, Path: p})
	}
	return snapshot.PublishRequest{Scope: scope(), Compatibility: compatibility(), Sources: sources}
}
func TestPublishedSnapshotMaterializesVerifiedImmutableArtifacts(t *testing.T) {
	store, _ := newStore(t)
	manifest, err := store.Publish(context.Background(), request(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Artifacts) != 3 || manifest.Artifacts[0].SHA256 != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" || manifest.Artifacts[0].Size != 3 {
		t.Fatalf("manifest lacks materialized integrity: %+v", manifest)
	}
	verified, err := store.OpenVerified(context.Background(), scope(), compatibility())
	if err != nil {
		t.Fatal(err)
	}
	defer verified.Close()
	data, err := os.ReadFile(verified.Paths[snapshot.Memory])
	if err != nil || string(data) != "abc" {
		t.Fatalf("verified memory: %q, %v", data, err)
	}
}

func TestPublicationRetryIsIdempotentButCannotReplaceSnapshot(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	req := request(t)
	first, err := store.Publish(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := store.Publish(ctx, req)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if again.UploadID != first.UploadID {
		t.Fatal("retry created a different publication")
	}
	if err = os.WriteFile(req.Sources[0].Path, []byte("different"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Publish(ctx, req); !errors.Is(err, snapshot.ErrConflict) {
		t.Fatalf("replacement: %v", err)
	}
}

func TestReferencePinsFenceDeletionAndDeletionFencesNewPins(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	if _, err := store.Publish(ctx, request(t)); err != nil {
		t.Fatal(err)
	}
	if err := store.AcquireReference(ctx, scope(), "child-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.AcquireReference(ctx, scope(), "child-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteUnreferenced(ctx, scope()); !errors.Is(err, snapshot.ErrReferenced) {
		t.Fatalf("deleted referenced snapshot: %v", err)
	}
	if err := store.ReleaseReference(ctx, scope(), "child-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteUnreferenced(ctx, scope()); err != nil {
		t.Fatal(err)
	}
	if err := store.AcquireReference(ctx, scope(), "child-b"); !errors.Is(err, snapshot.ErrDeleting) {
		t.Fatalf("acquired deleted snapshot: %v", err)
	}
	if _, err := store.OpenVerified(ctx, scope(), compatibility()); !errors.Is(err, snapshot.ErrNotFound) {
		t.Fatalf("deleted snapshot opened: %v", err)
	}
	if err := store.DeleteUnreferenced(ctx, scope()); err != nil {
		t.Fatalf("deletion retry: %v", err)
	}
}

type interruptedObjects struct {
	snapshot.Objects
	interrupt bool
}

func (o *interruptedObjects) Put(ctx context.Context, key string, r io.Reader, n int64, g string) (string, error) {
	if o.interrupt && strings.HasSuffix(key, "/memory") {
		return "", io.ErrUnexpectedEOF
	}
	return o.Objects.Put(ctx, key, r, n, g)
}
func TestInterruptedPublicationRemainsUnavailableAndExpiredStagingIsCollected(t *testing.T) {
	_, files := newStore(t)
	objects := &interruptedObjects{Objects: files, interrupt: true}
	store, err := snapshot.New(objects, snapshot.Options{TempDir: t.TempDir(), StagingTTL: 100 * time.Millisecond, OperationTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Publish(context.Background(), request(t)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("upload did not interrupt: %v", err)
	}
	if _, err = store.OpenVerified(context.Background(), scope(), compatibility()); !errors.Is(err, snapshot.ErrNotFound) {
		t.Fatalf("partial snapshot opened: %v", err)
	}
	if err = store.RecoverExpired(context.Background(), scope()); !errors.Is(err, snapshot.ErrConflict) {
		t.Fatalf("active upload swept: %v", err)
	}
	time.Sleep(180 * time.Millisecond)
	if err = store.RecoverExpired(context.Background(), scope()); err != nil {
		t.Fatal(err)
	}
	remaining, _, err := files.List(context.Background(), "tenants/tenant/projects/project/checkpoints/checkpoint/", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || !strings.HasSuffix(remaining[0].Key, "/lifetime") {
		t.Fatalf("abandoned artifacts remain: %+v", remaining)
	}
	if err = store.AcquireReference(context.Background(), scope(), "late-child"); !errors.Is(err, snapshot.ErrDeleting) {
		t.Fatalf("recovered failed upload became available: %v", err)
	}
}

func TestVerifiedOpenRejectsMissingCorruptAndIncompatibleSnapshots(t *testing.T) {
	for _, failure := range []string{"missing", "corrupt", "compatibility"} {
		t.Run(failure, func(t *testing.T) {
			store, objects := newStore(t)
			ctx := context.Background()
			manifest, err := store.Publish(ctx, request(t))
			if err != nil {
				t.Fatal(err)
			}
			expected := compatibility()
			want := snapshot.ErrIntegrity
			if failure == "compatibility" {
				expected.CPUIdentity = "other-cpu"
				want = snapshot.ErrIncompatible
			} else {
				a := manifest.Artifacts[0]
				r, g, err := objects.Get(ctx, a.Key)
				if err != nil {
					t.Fatal(err)
				}
				r.Close()
				if failure == "missing" {
					err = objects.Delete(ctx, a.Key, g)
					want = snapshot.ErrNotFound
				} else {
					_, err = objects.Put(ctx, a.Key, strings.NewReader("abd"), 3, g)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err = store.OpenVerified(ctx, scope(), expected); !errors.Is(err, want) {
				t.Fatalf("%s snapshot accepted: %v", failure, err)
			}
		})
	}
}

func TestConcurrentPinAndRetentionNeverDeleteAcquiredSnapshot(t *testing.T) {
	for i := 0; i < 12; i++ {
		store, _ := newStore(t)
		ctx := context.Background()
		if _, err := store.Publish(ctx, request(t)); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		pinResult := make(chan error, 1)
		deleteResult := make(chan error, 1)
		go func() { <-start; pinResult <- store.AcquireReference(ctx, scope(), "child") }()
		go func() { <-start; deleteResult <- store.DeleteUnreferenced(ctx, scope()) }()
		close(start)
		pinErr, deleteErr := <-pinResult, <-deleteResult
		if pinErr == nil {
			if !errors.Is(deleteErr, snapshot.ErrReferenced) {
				t.Fatalf("successful pin raced successful deletion: %v", deleteErr)
			}
			verified, err := store.OpenVerified(ctx, scope(), compatibility())
			if err != nil {
				t.Fatalf("live pin lost artifacts: %v", err)
			}
			verified.Close()
		} else if !errors.Is(pinErr, snapshot.ErrDeleting) || deleteErr != nil {
			t.Fatalf("race result: pin=%v deletion=%v", pinErr, deleteErr)
		}
	}
}

func TestEmptyArtifactCannotBecomePublishedSnapshot(t *testing.T) {
	store, _ := newStore(t)
	req := request(t)
	if err := os.Truncate(req.Sources[0].Path, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish(context.Background(), req); !errors.Is(err, snapshot.ErrInvalid) {
		t.Fatalf("published empty VM artifact: %v", err)
	}
}

func TestReleaseUnacquiredReferenceIsSafeDuringRecovery(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	// Allocation recovery can run after its journal entry was persisted but
	// before either checkpoint discovery or the reference acquisition happened.
	if err := store.ReleaseReference(ctx, scope(), "unacquired-child"); err != nil {
		t.Fatalf("release before acquisition blocks recovery: %v", err)
	}
	if err := store.AcquireReference(ctx, scope(), "unacquired-child"); !errors.Is(err, snapshot.ErrNotFound) {
		t.Fatalf("missing checkpoint accepted a pin: %v", err)
	}
	if _, err := store.Publish(ctx, request(t)); err != nil {
		t.Fatal(err)
	}
	if err := store.AcquireReference(ctx, scope(), "live-child"); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseReference(ctx, scope(), "unacquired-child"); err != nil {
		t.Fatalf("release of unknown reference blocks recovery: %v", err)
	}
	if err := store.DeleteUnreferenced(ctx, scope()); !errors.Is(err, snapshot.ErrReferenced) {
		t.Fatalf("unknown release removed a live child's pin: %v", err)
	}
}

func TestRecoveryGraceCoversPublicationLeaseAndInflightRequest(t *testing.T) {
	_, objects := newStore(t)
	store, err := snapshot.New(objects, snapshot.Options{StagingTTL: 2 * time.Minute, OperationTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if got := store.RecoveryGrace(); got != 3*time.Minute {
		t.Fatalf("unsafe missing-publication recovery grace: %v", got)
	}
}

func TestRestoreMaterializationUsesOnlyItsOwnerCache(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	if _, err := store.Publish(ctx, request(t)); err != nil {
		t.Fatal(err)
	}
	owner := t.TempDir()
	if err := os.Chmod(owner, 0700); err != nil {
		t.Fatal(err)
	}
	verified, err := store.OpenVerifiedIn(ctx, scope(), compatibility(), owner)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(owner, "other-live-owner")
	if err := os.WriteFile(sentinel, []byte("retained"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range verified.Paths {
		if filepath.Dir(filepath.Dir(path)) != owner {
			t.Fatalf("materialization escaped owner cache: %s", path)
		}
	}
	if err := verified.Close(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "retained" {
		t.Fatalf("materialization cleanup affected other owner: %q %v", data, err)
	}
}

func TestPublishPromptlyRejectsFIFOAndSymlinkSources(t *testing.T) {
	for _, kind := range []string{"fifo", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			store, _ := newStore(t)
			req := request(t)
			unsafe := filepath.Join(t.TempDir(), kind)
			if kind == "fifo" {
				if err := unix.Mkfifo(unsafe, 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink(req.Sources[0].Path, unsafe); err != nil {
				t.Fatal(err)
			}
			req.Sources[0].Path = unsafe
			done := make(chan error, 1)
			go func() { _, err := store.Publish(context.Background(), req); done <- err }()
			select {
			case err := <-done:
				if !errors.Is(err, snapshot.ErrInvalid) {
					t.Fatalf("unsafe source was accepted: %v", err)
				}
			case <-time.After(250 * time.Millisecond):
				t.Fatal("source open blocked publication instead of rejecting promptly")
			}
		})
	}
}

func TestPublicationTelemetryReportsOutcomeWithoutSourcePaths(t *testing.T) {
	_, objects := newStore(t)
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	store, err := snapshot.New(objects, snapshot.Options{TempDir: t.TempDir(), Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	req := request(t)
	if _, err = store.Publish(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err = json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil {
		t.Fatal(err)
	}
	if record["msg"] != "snapshot_publish" || record["outcome"] != "success" || record["artifact_bytes"] != float64(9) || record["checkpoint_id"] != "checkpoint" {
		t.Fatalf("missing safe publication telemetry: %+v", record)
	}
	for _, src := range req.Sources {
		if strings.Contains(output.String(), src.Path) {
			t.Fatal("source path exposed in telemetry")
		}
	}
	output.Reset()
	privatePath := filepath.Join(t.TempDir(), "private-credential-source")
	req.Sources[0].Path = privatePath
	if _, err = store.Publish(context.Background(), req); !errors.Is(err, snapshot.ErrInvalid) {
		t.Fatalf("unsafe source outcome: %v", err)
	}
	if strings.Contains(output.String(), privatePath) {
		t.Fatal("failed publication exposed source path")
	}
	if err = json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil {
		t.Fatal(err)
	}
	if record["outcome"] != "invalid" {
		t.Fatalf("failed publication outcome missing: %+v", record)
	}
}

func TestPublishRejectsOversizedSourceBeforeReadingSparseFile(t *testing.T) {
	store, _ := newStore(t)
	req := request(t)
	if err := os.Truncate(req.Sources[0].Path, (16<<30)+1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish(context.Background(), req); !errors.Is(err, snapshot.ErrInvalid) {
		t.Fatalf("oversized source accepted: %v", err)
	}
}

type replacedSourceObjects struct {
	snapshot.Objects
	source   string
	replaced bool
}

func (o *replacedSourceObjects) Put(ctx context.Context, key string, r io.Reader, n int64, generation string) (string, error) {
	if !o.replaced && strings.HasSuffix(key, "/lifetime") {
		o.replaced = true
		if err := os.Rename(o.source, o.source+".captured"); err != nil {
			return "", err
		}
		if err := os.WriteFile(o.source, []byte("replaced-path"), 0600); err != nil {
			return "", err
		}
	}
	return o.Objects.Put(ctx, key, r, n, generation)
}
func TestPublicationKeepsCapturedDescriptorWhenSourcePathIsReplaced(t *testing.T) {
	_, objects := newStore(t)
	req := request(t)
	boundary := &replacedSourceObjects{Objects: objects, source: req.Sources[0].Path}
	store, err := snapshot.New(boundary, snapshot.Options{TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Publish(context.Background(), req); err != nil {
		t.Fatalf("captured descriptor lost after pathname replacement: %v", err)
	}
	verified, err := store.OpenVerified(context.Background(), scope(), compatibility())
	if err != nil {
		t.Fatal(err)
	}
	defer verified.Close()
	data, err := os.ReadFile(verified.Paths[req.Sources[0].Kind])
	if err != nil || string(data) != "abc" {
		t.Fatalf("publication read replacement path: %q %v", data, err)
	}
}
