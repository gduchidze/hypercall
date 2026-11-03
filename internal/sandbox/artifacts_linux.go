//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/gduchidze/hypercall/internal/sandbox/hostdoctor"
)

type assets struct{ firecracker, jailer, kernel, rootfs string }

func decodeFile(path string, v any) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() > 1<<20 {
		return errors.New("configuration must be regular and <=1MiB")
	}
	d := json.NewDecoder(io.LimitReader(f, (1<<20)+1))
	d.DisallowUnknownFields()
	if err = d.Decode(v); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("configuration must contain one JSON object")
	}
	return nil
}
func stageArtifact(ctx context.Context, a hostdoctor.Artifact, dest string, executable bool) error {
	digest, e := hex.DecodeString(a.SHA256)
	if e != nil || len(digest) != 32 || !filepath.IsAbs(a.Path) {
		return fmt.Errorf("%w: absolute artifact and SHA256 required", ErrInvalid)
	}
	f, e := os.OpenFile(a.Path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return e
	}
	if !st.Mode().IsRegular() || st.Size() == 0 || st.Size() > 2<<30 || st.Mode().Perm()&0022 != 0 {
		return errors.New("artifact must be nonempty regular <=2GiB and not group/world writable")
	}
	if executable && st.Mode().Perm()&0111 == 0 {
		return errors.New("artifact not executable")
	}
	out, e := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	h := sha256.New()
	buf := make([]byte, 1<<20)
	var n int64
	for {
		if e = ctx.Err(); e != nil {
			break
		}
		var k int
		k, e = f.Read(buf)
		if k > 0 {
			if n+int64(k) > 2<<30 {
				e = errors.New("artifact grew beyond bound")
				break
			}
			var w int
			w, e = out.Write(buf[:k])
			if e != nil {
				break
			}
			_, _ = h.Write(buf[:w])
			n += int64(w)
		}
		if e != nil {
			break
		}
	}
	if e == io.EOF {
		e = nil
	}
	if e == nil && hex.EncodeToString(h.Sum(nil)) != strings.ToLower(a.SHA256) {
		e = errors.New("artifact SHA256 mismatch")
	}
	if e == nil {
		e = out.Sync()
	}
	if closeErr := out.Close(); e == nil {
		e = closeErr
	}
	if e != nil {
		_ = os.Remove(dest)
		return e
	}
	mode := os.FileMode(0400)
	if executable {
		// Jailer copies the executable without chowning it before dropping uid;
		// the immutable VMM must remain executable by the non-root jail uid.
		mode = 0555
	}
	return os.Chmod(dest, mode)
}
func (b *Backend) stage(ctx context.Context) error {
	var cfg hostdoctor.ArtifactConfig
	if err := decodeFile(b.cfg.ArtifactConfigPath, &cfg); err != nil {
		return fmt.Errorf("artifact config: %w", err)
	}
	if cfg.SchemaVersion != 1 || cfg.Firecracker.Version != "Firecracker v1.14.0" || cfg.Jailer.Version != "Jailer v1.14.0" {
		return fmt.Errorf("%w: official Firecracker and jailer 1.14.0 exact pins required", ErrInvalid)
	}
	dir, err := os.MkdirTemp(b.cfg.StateDir, "assets-")
	if err != nil {
		return err
	}
	b.assetDir = dir
	manifest := filepath.Join(dir, "template.json")
	if err = stageArtifact(ctx, cfg.Template, manifest, false); err != nil {
		return err
	}
	var m hostdoctor.TemplateManifest
	if err = decodeFile(manifest, &m); err != nil {
		return err
	}
	if m.SchemaVersion != 1 || m.Architecture != runtime.GOARCH {
		return fmt.Errorf("%w: template schema or architecture", ErrInvalid)
	}
	for _, item := range []struct {
		a          hostdoctor.Artifact
		name       string
		executable bool
	}{{cfg.Firecracker, "firecracker", true}, {cfg.Jailer, "jailer", true}, {m.Kernel, "kernel", false}, {m.RootFS, "rootfs.ext4", false}, {m.GuestAgent, "hypercall-guest", false}} {
		if err = stageArtifact(ctx, item.a, filepath.Join(dir, item.name), item.executable); err != nil {
			return fmt.Errorf("stage %s: %w", item.name, err)
		}
	}
	for _, item := range []struct{ name, version string }{{"firecracker", cfg.Firecracker.Version}, {"jailer", cfg.Jailer.Version}} {
		timeout, cancel := context.WithTimeout(ctx, 5*time.Second)
		cmd := exec.CommandContext(timeout, filepath.Join(dir, item.name), "--version")
		cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}
		var output versionOutput
		cmd.Stdout = &output
		cmd.Stderr = io.Discard
		runErr := cmd.Run()
		out := output.Bytes()
		cancel()
		if runErr != nil || strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]) != item.version {
			return fmt.Errorf("%w: pinned %s version failed", ErrInvalid, item.name)
		}
	}
	b.assets = assets{filepath.Join(dir, "firecracker"), filepath.Join(dir, "jailer"), filepath.Join(dir, "kernel"), filepath.Join(dir, "rootfs.ext4")}
	return nil
}

// Version diagnostics are bounded independently from the immutable hash check.
type versionOutput struct{ bytes.Buffer }

func (v *versionOutput) Write(p []byte) (int, error) {
	if v.Len()+len(p) > 64<<10 {
		return 0, errors.New("version output exceeded 64KiB")
	}
	return v.Buffer.Write(p)
}
