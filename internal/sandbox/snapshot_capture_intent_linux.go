//go:build linux

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/gduchidze/hypercall/internal/snapshot"
)

type captureIntent struct {
	Scope        snapshot.Scope `json:"scope"`
	RecoverAfter time.Time      `json:"recover_after"`
}

func captureIntentName(scope snapshot.Scope) string {
	data, _ := json.Marshal(scope)
	return digest(data) + ".json"
}
func (b *Backend) writeCaptureIntent(scope snapshot.Scope) (string, error) {
	name := captureIntentName(scope)
	path := filepath.Join("captures", name)
	var old captureIntent
	err := decodeFile(filepath.Join(b.cfg.StateDir, path), &old)
	if err == nil {
		if old.Scope != scope {
			return "", errors.New("capture intent identity mismatch")
		}
		return path, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	entries, e := readDir(b.root, "captures")
	if e != nil {
		return "", e
	}
	if len(entries) >= 4096 {
		return "", errors.New("host capture intent capacity exceeded")
	}
	intent := captureIntent{Scope: scope, RecoverAfter: time.Now().Add(b.cfg.SnapshotStore.RecoveryGrace())}
	pending := filepath.Join(b.cfg.StateDir, "captures", name+".pending")
	if err = writeJSON(pending, intent, 0600); err != nil {
		return "", err
	}
	if err = b.root.Rename(filepath.Join("captures", name+".pending"), path); err != nil {
		return "", err
	}
	return path, b.syncCaptureIntents()
}
func (b *Backend) syncCaptureIntents() error {
	dir, err := b.root.Open("captures")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (b *Backend) clearCaptureIntent(path string) error {
	if err := b.root.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return b.syncCaptureIntents()
}
func (b *Backend) recoverCaptureIntents(ctx context.Context) error {
	entries, err := readDir(b.root, "captures")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join("captures", entry.Name())
		if filepath.Ext(entry.Name()) == ".pending" {
			if err = b.clearCaptureIntent(path); err != nil {
				return err
			}
			continue
		}
		var intent captureIntent
		if err = decodeFile(filepath.Join(b.cfg.StateDir, path), &intent); err != nil {
			return err
		}
		if entry.Name() != captureIntentName(intent.Scope) || !identifier.MatchString(intent.Scope.TenantID) || !identifier.MatchString(intent.Scope.ProjectID) || !identifier.MatchString(intent.Scope.CheckpointID) || intent.RecoverAfter.IsZero() {
			return errors.New("invalid scoped capture intent")
		}
		if b.cfg.SnapshotStore == nil {
			return errors.New("snapshot store required for unfinished capture recovery")
		}
		err = b.cfg.SnapshotStore.RecoverExpired(ctx, intent.Scope)
		if errors.Is(err, snapshot.ErrConflict) {
			continue
		}
		if errors.Is(err, snapshot.ErrNotFound) && time.Now().Before(intent.RecoverAfter) {
			continue
		}
		if err != nil && !errors.Is(err, snapshot.ErrNotFound) {
			return err
		}
		if err = b.clearCaptureIntent(path); err != nil {
			return err
		}
	}
	return nil
}
