//go:build linux

package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const maxFences = 4096

type fence struct {
	TenantID  string `json:"tenant_id"`
	ProjectID string `json:"project_id"`
	SandboxID string `json:"sandbox_id"`
	Epoch     uint64 `json:"epoch"`
}

func fenceFor(a Allocation) fence { return fence{a.TenantID, a.ProjectID, a.SandboxID, a.Epoch} }
func (f fence) name() string {
	data, _ := json.Marshal([]string{f.TenantID, f.ProjectID, f.SandboxID})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]) + ".json"
}
func (b *Backend) loadFences() error {
	entries, err := readDir(b.root, "fences")
	if err != nil {
		return err
	}
	if len(entries) > maxFences {
		return errors.New("host-local epoch fence capacity exceeded")
	}
	for _, entry := range entries {
		// Interrupted writes publish no admission; the previous durable fence is
		// authoritative. The private directory is writable only by this host.
		if entry.Name() == ".pending" {
			if err = b.root.Remove(filepath.Join("fences", entry.Name())); err != nil {
				return err
			}
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 4096 {
			return errors.New("epoch fence must be root-owned regular0600 <=4KiB")
		}
		var f fence
		if err = decodeFile(filepath.Join(b.cfg.StateDir, "fences", entry.Name()), &f); err != nil {
			return err
		}
		if !identifier.MatchString(f.TenantID) || !identifier.MatchString(f.ProjectID) || !identifier.MatchString(f.SandboxID) || f.Epoch == 0 || entry.Name() != f.name() {
			return errors.New("invalid host-local epoch fence")
		}
		b.fences[entry.Name()] = f.Epoch
	}
	return nil
}

// advanceFence is called while the allocation-map mutex is held. Admission
// consumes the epoch even if boot subsequently fails; retry requires a fresh
// epoch. This is host-local fencing, not ownership of a durable product lease.
func (b *Backend) advanceFence(a Allocation) error {
	f := fenceFor(a)
	name := f.name()
	highest, exists := b.fences[name]
	if a.Epoch <= highest {
		return fmt.Errorf("%w: stale host-local epoch", ErrConflict)
	}
	if !exists && len(b.fences) >= maxFences {
		return fmt.Errorf("%w: host-local fence capacity exhausted", ErrUnavailable)
	}
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	temp := filepath.Join("fences", ".pending")
	out, err := b.root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	_, err = out.Write(data)
	if err == nil {
		err = out.Sync()
	}
	err = errors.Join(err, out.Close())
	if err != nil {
		_ = b.root.Remove(temp)
		return err
	}
	if err = b.root.Rename(temp, filepath.Join("fences", name)); err != nil {
		_ = b.root.Remove(temp)
		return err
	}
	// Retain the consumed epoch in memory even if the following fsync fails.
	b.fences[name] = a.Epoch
	dir, err := b.root.Open("fences")
	if err != nil {
		return err
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return err
	}
	b.fences[name] = a.Epoch
	return nil
}
