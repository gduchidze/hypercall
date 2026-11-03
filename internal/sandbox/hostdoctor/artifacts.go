package hostdoctor

import (
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
	"sync"
	"time"
)

type Artifact struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Version string `json:"version,omitempty"`
}

type ArtifactConfig struct {
	SchemaVersion int      `json:"schema_version"`
	Firecracker   Artifact `json:"firecracker"`
	Jailer        Artifact `json:"jailer"`
	Template      Artifact `json:"template"`
}

type TemplateManifest struct {
	SchemaVersion int      `json:"schema_version"`
	Architecture  string   `json:"architecture"`
	Kernel        Artifact `json:"kernel"`
	RootFS        Artifact `json:"rootfs"`
	GuestAgent    Artifact `json:"guest_agent"`
}

func artifactChecks(ctx context.Context, path string) []Check {
	if path == "" {
		return []Check{blocked("runtime.configuration", "provide --config with explicit pinned Firecracker, jailer and template artifacts")}
	}
	var config ArtifactConfig
	if err := decodeFile(path, &config); err != nil {
		return []Check{blocked("runtime.configuration", err.Error())}
	}
	if config.SchemaVersion != 1 {
		return []Check{blocked("runtime.configuration", "unsupported schema_version; require 1")}
	}
	checks := []Check{passed("runtime.configuration", "parsed explicit runtime artifact configuration schema 1")}
	for _, binary := range []struct {
		name     string
		artifact Artifact
	}{{"firecracker", config.Firecracker}, {"jailer", config.Jailer}} {
		id := "runtime." + binary.name
		check, file := checkArtifact(ctx, id+".integrity", binary.artifact, true)
		checks = append(checks, check)
		if check.Status != "passed" {
			continue
		}
		checks = append(checks, checkVersion(ctx, id+".version", binary.artifact, file))
		file.Close()
	}
	check, manifestFile := checkArtifact(ctx, "template.manifest.integrity", config.Template, false)
	checks = append(checks, check)
	if check.Status != "passed" {
		return checks
	}
	defer manifestFile.Close()
	var manifest TemplateManifest
	if err := decodeJSON(manifestFile, &manifest); err != nil {
		return append(checks, blocked("template.manifest", err.Error()))
	}
	if manifest.SchemaVersion != 1 || manifest.Architecture != runtime.GOARCH {
		return append(checks, blocked("template.manifest", "require schema_version 1 and architecture matching host "+runtime.GOARCH))
	}
	checks = append(checks, passed("template.manifest", "manifest schema and host architecture match; hashes establish integrity, not boot compatibility"))
	for _, asset := range []struct {
		name     string
		artifact Artifact
	}{{"kernel", manifest.Kernel}, {"rootfs", manifest.RootFS}, {"guest_agent", manifest.GuestAgent}} {
		check, file := checkArtifact(ctx, "template."+asset.name+".integrity", asset.artifact, false)
		checks = append(checks, check)
		if file != nil {
			file.Close()
		}
	}
	return checks
}

func decodeFile(path string, result any) error {
	file, err := openRegular(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()
	return decodeJSON(file, result)
}

func decodeJSON(file *os.File, result any) error {
	path := file.Name()
	stat, err := file.Stat()
	if err != nil {
		return err
	}
	if !stat.Mode().IsRegular() || stat.Size() > 1<<20 {
		return fmt.Errorf("%s must be a regular JSON file no larger than 1 MiB", path)
	}
	decoder := json.NewDecoder(io.LimitReader(file, (1<<20)+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s must contain exactly one JSON value", path)
	}
	return nil
}

func checkArtifact(ctx context.Context, id string, artifact Artifact, executable bool) (Check, *os.File) {
	if !filepath.IsAbs(artifact.Path) {
		return blocked(id, "artifact path must be explicitly configured and absolute"), nil
	}
	digest, err := hex.DecodeString(artifact.SHA256)
	if err != nil || len(digest) != sha256.Size {
		return blocked(id, "expected sha256 must be exactly 64 hexadecimal characters"), nil
	}
	file, err := openRegular(artifact.Path)
	if err != nil {
		return blocked(id, err.Error()), nil
	}
	verified := false
	defer func() {
		if !verified {
			file.Close()
		}
	}()
	stat, err := file.Stat()
	if err != nil {
		return blocked(id, err.Error()), nil
	}
	if !stat.Mode().IsRegular() || stat.Size() == 0 {
		return blocked(id, "artifact must be a nonempty regular file"), nil
	}
	if executable && stat.Mode().Perm()&0111 == 0 {
		return blocked(id, "configured binary is not executable"), nil
	}
	if stat.Mode().Perm()&0022 != 0 {
		return blocked(id, "artifact is writable by group or others; remove those write permissions"), nil
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, contextReader{ctx, file}); err != nil {
		return blocked(id, "hash artifact: "+err.Error()), nil
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	check := passed(id, "verified explicitly configured artifact SHA-256")
	if !strings.EqualFold(actual, artifact.SHA256) {
		check = blocked(id, "artifact SHA-256 mismatch; rejected before execution or manifest use")
	}
	check.Evidence = map[string]string{"path": artifact.Path, "sha256": actual, "expected_sha256": strings.ToLower(artifact.SHA256)}
	if check.Status != "passed" {
		return check, nil
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return blocked(id, "rewind verified artifact: "+err.Error()), nil
	}
	verified = true
	return check, file
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func checkVersion(ctx context.Context, id string, artifact Artifact, file *os.File) Check {
	if artifact.Version == "" || strings.TrimSpace(artifact.Version) != artifact.Version {
		return blocked(id, "configure exact nonempty --version output, without leading or trailing whitespace")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	executable, extra, cleanup, err := prepareExecutable(ctx, file, artifact.SHA256)
	if err != nil {
		return blocked(id, "prepare verified executable: "+err.Error())
	}
	cmd := exec.CommandContext(ctx, executable, "--version")
	cmd.ExtraFiles = extra
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	cmd.WaitDelay = 500 * time.Millisecond
	var output limitedOutput
	cmd.Stdout = &output
	cmd.Stderr = &output
	runErr := cmd.Run()
	if err := cleanup(); err != nil {
		return blocked(id, "clean up private executable: "+err.Error())
	}
	if err := runErr; err != nil {
		return blocked(id, "configured artifact --version failed: "+err.Error())
	}
	actual := strings.TrimSpace(string(output.data))
	check := passed(id, "configured artifact --version matches exact pinned output")
	if actual != artifact.Version {
		check = blocked(id, "configured artifact --version does not match pinned output")
	}
	check.Evidence = map[string]string{"expected_version": artifact.Version, "version": actual}
	return check
}

type limitedOutput struct {
	mu   sync.Mutex
	data []byte
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	const max = 64 << 10
	if len(b.data)+len(p) > max {
		return 0, errors.New("version output exceeded 64 KiB")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}
