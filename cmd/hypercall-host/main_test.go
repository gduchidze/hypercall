package main_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

var hostBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "hypercall-host-test-")
	if err != nil {
		panic(err)
	}
	hostBinary = filepath.Join(dir, "hypercall-host")
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", hostBinary, ".")
	if output, err := cmd.CombinedOutput(); err != nil {
		panic(string(output) + err.Error())
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type report struct {
	Scope        string `json:"scope"`
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Ready        bool   `json:"ready"`
	Checks       []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	} `json:"checks"`
	Blockers []string `json:"blockers"`
}

func runDoctor(t *testing.T, args ...string) report {
	t.Helper()
	// Artifact fixtures are command/file-system tests. An ordinary temporary
	// directory deliberately prevents them from qualifying a real KVM host.
	cmd := exec.Command(hostBinary, append([]string{"doctor", "--json", "--cgroup-dir", t.TempDir()}, args...)...)
	output, err := cmd.Output()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("want blocked exit 1, got %v; output %s", err, output)
	}
	var result report
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("invalid JSON: %s: %v", output, err)
	}
	return result
}

func requireCheck(t *testing.T, r report, id, status string) {
	t.Helper()
	for _, check := range r.Checks {
		if check.ID == id {
			if check.Status != status {
				t.Fatalf("%s = %+v; want %s", id, check, status)
			}
			return
		}
	}
	t.Fatalf("missing check %s in %+v", id, r)
}

func TestDoctorBlocksMissingRuntimeConfiguration(t *testing.T) {
	r := runDoctor(t)
	if r.Ready || r.Scope != "host-prerequisites" || r.OS != runtime.GOOS || r.Architecture != runtime.GOARCH {
		t.Fatalf("incorrect host qualification report: %+v", r)
	}
	requireCheck(t, r, "runtime.configuration", "blocked")
	if len(r.Blockers) == 0 {
		t.Fatal("missing actionable blockers")
	}
}

func TestDoctorBlocksNativeNonLinuxHost(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("non-Linux host behavior; Linux prerequisites run in the opt-in KVM suite")
	}
	r := runDoctor(t)
	for _, id := range []string{"host.platform", "host.kvm", "host.cgroups", "host.namespaces"} {
		requireCheck(t, r, id, "blocked")
	}
}

func artifactFixture(t *testing.T) (string, map[string]any) {
	t.Helper()
	dir := t.TempDir()
	artifact := func(name, contents string, executable bool) map[string]any {
		path := filepath.Join(dir, name)
		mode := os.FileMode(0600)
		if executable {
			mode = 0700
		}
		if err := os.WriteFile(path, []byte(contents), mode); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(contents))
		return map[string]any{"path": path, "sha256": fmt.Sprintf("%x", sum)}
	}
	fc := artifact("firecracker", "#!/bin/sh\nprintf 'Firecracker vfixture\\n'\n", true)
	fc["version"] = "Firecracker vfixture"
	jailer := artifact("jailer", "#!/bin/sh\nprintf 'Jailer vfixture\\n'\n", true)
	jailer["version"] = "Jailer vfixture"
	manifest := map[string]any{
		"schema_version": 1, "architecture": runtime.GOARCH,
		"kernel":      artifact("kernel", "known kernel fixture", false),
		"rootfs":      artifact("rootfs", "known filesystem fixture", false),
		"guest_agent": artifact("guest-agent", "known guest fixture", false),
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	config := map[string]any{"schema_version": 1, "firecracker": fc, "jailer": jailer, "template": artifact("template.json", string(manifestJSON), false)}
	return dir, config
}

func writeConfig(t *testing.T, dir string, config map[string]any) string {
	t.Helper()
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "runtime.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDoctorRejectsCorruptBinaryBeforeExecutingIt(t *testing.T) {
	dir, config := artifactFixture(t)
	marker := filepath.Join(dir, "executed")
	if err := os.WriteFile(filepath.Join(dir, "firecracker"), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	r := runDoctor(t, "--config", writeConfig(t, dir, config))
	requireCheck(t, r, "runtime.firecracker.integrity", "blocked")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("corrupt binary was executed: %v", err)
	}
}

func TestDoctorVerifiesConfiguredArtifactsWithoutClaimingBoot(t *testing.T) {
	dir, config := artifactFixture(t)
	r := runDoctor(t, "--config", writeConfig(t, dir, config))
	for _, id := range []string{"runtime.configuration", "runtime.firecracker.integrity", "runtime.firecracker.version", "runtime.jailer.integrity", "runtime.jailer.version", "template.manifest.integrity", "template.manifest", "template.kernel.integrity", "template.rootfs.integrity", "template.guest_agent.integrity"} {
		requireCheck(t, r, id, "passed")
	}
	if r.Ready {
		t.Fatal("artifact-only fixtures must not qualify a real host")
	}
}

func TestDoctorDetectsChangedTemplateDisk(t *testing.T) {
	dir, config := artifactFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "rootfs"), []byte("tampered disk"), 0600); err != nil {
		t.Fatal(err)
	}
	r := runDoctor(t, "--config", writeConfig(t, dir, config))
	requireCheck(t, r, "template.manifest.integrity", "passed")
	requireCheck(t, r, "template.rootfs.integrity", "blocked")
}

func TestDoctorRejectsUnexpectedBinaryVersion(t *testing.T) {
	dir, config := artifactFixture(t)
	config["firecracker"].(map[string]any)["version"] = "Firecracker vdifferent"
	r := runDoctor(t, "--config", writeConfig(t, dir, config))
	requireCheck(t, r, "runtime.firecracker.integrity", "passed")
	requireCheck(t, r, "runtime.firecracker.version", "blocked")
}

func TestDoctorRejectsUnknownConfigurationFields(t *testing.T) {
	dir, config := artifactFixture(t)
	config["download_latest"] = true
	r := runDoctor(t, "--config", writeConfig(t, dir, config))
	requireCheck(t, r, "runtime.configuration", "blocked")
}

func TestDoctorRejectsFIFOsWithoutBlocking(t *testing.T) {
	for _, kind := range []string{"configuration", "binary"} {
		t.Run(kind, func(t *testing.T) {
			dir, config := artifactFixture(t)
			fifo := filepath.Join(dir, "fifo")
			if err := syscall.Mkfifo(fifo, 0600); err != nil {
				t.Fatal(err)
			}
			configPath := fifo
			checkID := "runtime.configuration"
			if kind == "binary" {
				config["firecracker"].(map[string]any)["path"] = fifo
				configPath = writeConfig(t, dir, config)
				checkID = "runtime.firecracker.integrity"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, hostBinary, "doctor", "--json", "--cgroup-dir", dir, "--config", configPath)
			output, err := cmd.Output()
			if ctx.Err() != nil {
				t.Fatalf("doctor blocked opening %s FIFO until subprocess deadline: %v", kind, ctx.Err())
			}
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
				t.Fatalf("want blocked exit 1, got %v: %s", err, output)
			}
			var result report
			if err := json.Unmarshal(output, &result); err != nil {
				t.Fatalf("invalid JSON: %s: %v", output, err)
			}
			requireCheck(t, result, checkID, "blocked")
		})
	}
}

func TestDoctorDoesNotExecuteReplacementBinaryPath(t *testing.T) {
	dir, config := artifactFixture(t)
	binaryPath := filepath.Join(dir, "firecracker")
	interpreter := filepath.Join(dir, "interpreter")
	replacement := filepath.Join(dir, "replacement")
	marker := filepath.Join(dir, "unchecked-executable-ran")
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	// The interpreter replaces the configured pathname after the kernel selects
	// the script but before its contents are consumed. The script passed to the
	// interpreter must still refer to the verified file, despite that replacement.
	interpreterSource := `package main
import ("os"; "path/filepath"; "syscall")
func main() {
    executable, err := os.Executable(); if err != nil { panic(err) }
    dir := filepath.Dir(executable)
    binary := filepath.Join(dir, "firecracker")
    if err := os.Rename(binary, binary+".original"); err != nil { panic(err) }
    data, err := os.ReadFile(filepath.Join(dir, "replacement")); if err != nil { panic(err) }
    if err := os.WriteFile(binary, data, 0700); err != nil { panic(err) }
    if err := syscall.Exec("/bin/sh", []string{"sh", os.Args[1], "--version"}, os.Environ()); err != nil { panic(err) }
}
`
	interpreterGo := filepath.Join(dir, "interpreter.go")
	if err := os.WriteFile(interpreterGo, []byte(interpreterSource), 0600); err != nil {
		t.Fatal(err)
	}
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", interpreter, interpreterGo)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build native interpreter fixture: %v: %s", err, output)
	}
	replacementScript := "#!/bin/sh\ntouch " + quote(marker) + "\nprintf 'Firecracker vfixture\\n'\n"
	if err := os.WriteFile(replacement, []byte(replacementScript), 0700); err != nil {
		t.Fatal(err)
	}
	verifiedScript := "#!" + interpreter + "\nprintf 'Firecracker vfixture\\n'\n"
	if err := os.WriteFile(binaryPath, []byte(verifiedScript), 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(verifiedScript))
	config["firecracker"].(map[string]any)["sha256"] = fmt.Sprintf("%x", sum)
	r := runDoctor(t, "--config", writeConfig(t, dir, config))
	requireCheck(t, r, "runtime.firecracker.integrity", "passed")
	requireCheck(t, r, "runtime.firecracker.version", "passed")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("pathname replacement executed unchecked bytes: %v", err)
	}
}
