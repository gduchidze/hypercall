//go:build kvm

package kvm_test

import (
	"context"
	"encoding/json"
	"os"
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
