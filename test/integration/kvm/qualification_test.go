//go:build kvm

package kvm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/gduchidze/hypercall/internal/sandbox/hostdoctor"
)

// TestHostPrerequisites performs real host operations. It deliberately does not
// claim boot, guest commands/files, sandbox isolation, or Phase 1A acceptance.
func TestHostPrerequisites(t *testing.T) {
	if os.Getenv("HYPERCALL_KVM_TEST") != "1" {
		t.Skip("real KVM qualification not requested: set HYPERCALL_KVM_TEST=1 and supply explicit artifact configuration; this skip is not Phase 1A acceptance")
	}
	config := os.Getenv("HYPERCALL_HOST_CONFIG")
	if config == "" {
		t.Fatal("opted-in KVM qualification requires HYPERCALL_HOST_CONFIG; missing artifacts must not silently skip")
	}
	cgroups := os.Getenv("HYPERCALL_CGROUP_DIR")
	if cgroups == "" {
		cgroups = "/sys/fs/cgroup"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report := hostdoctor.Run(ctx, hostdoctor.Options{ConfigPath: config, CgroupDir: cgroups})
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(data))
	if !report.Ready {
		t.Fatalf("host prerequisites blocked: %v", report.Blockers)
	}
	for _, check := range report.Checks {
		if check.ID == "host.kvm" && check.Evidence["api_version"] == "12" {
			return
		}
	}
	t.Fatal("host qualification did not include a real KVM API version 12 result")
}

// TestFullPhase1A runs the privileged production backend through its standalone
// operator. Opting in is an assertion that real artifacts and KVM are available:
// missing configuration, binary, Python, tools or checks fail rather than skip.
func TestFullPhase1A(t *testing.T) {
	if os.Getenv("HYPERCALL_KVM_TEST") != "1" {
		t.Skip("set HYPERCALL_KVM_TEST=1 to require the full real KVM gate")
	}
	required := func(name string) string {
		t.Helper()
		v := os.Getenv(name)
		if v == "" {
			t.Fatalf("opted-in full qualification requires %s", name)
		}
		return v
	}
	binary := required("HYPERCALL_TESTDRIVER")
	config := required("HYPERCALL_HOST_CONFIG")
	cgroup := required("HYPERCALL_CGROUP_DIR")
	state := required("HYPERCALL_QUALIFICATION_STATE")
	probe := required("HYPERCALL_PYTHON_PROBE")
	python := required("HYPERCALL_PYTHON")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	args := []string{"qualify", "--config", config, "--state-dir", state, "--cgroup-dir", cgroup, "--python", python, "--python-probe", probe}
	if report := os.Getenv("HYPERCALL_QUALIFICATION_REPORT"); report != "" {
		args = append(args, "--output", report)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if len(output) > 8<<20 {
		t.Fatal("qualification report exceeds 8 MiB")
	}
	t.Log(string(output))
	if err != nil {
		t.Fatalf("real KVM qualification failed: %v\n%s", err, stderr.String())
	}
	var report struct {
		SchemaVersion int `json:"schema_version"`
		Checks        []struct {
			ID     string `json:"id"`
			Passed bool   `json:"passed"`
			Error  string `json:"error"`
		} `json:"checks"`
	}
	if err = json.Unmarshal(output, &report); err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != 1 {
		t.Fatalf("unexpected report schema %d", report.SchemaVersion)
	}
	seen := map[string]bool{}
	for _, c := range report.Checks {
		if !c.Passed {
			t.Errorf("%s failed: %s", c.ID, c.Error)
		}
		seen[c.ID] = c.Passed
	}
	for _, id := range []string{"host-prerequisites", "boot", "commands", "files", "python-vsock", "authentication", "scope-epoch", "concurrent-ttl", "cancellation", "deadline", "disconnect", "resource-limits", "deny-network", "vm-isolation", "guest-failure", "host-crash-recovery", "artifact-failures", "cleanup", "log-redaction"} {
		if !seen[id] {
			t.Errorf("required acceptance gate missing or failed: %s", id)
		}
	}
}
