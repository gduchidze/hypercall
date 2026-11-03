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
)

// TestPhase1B requires real snapshot artifacts, Python memory continuity, and
// privileged Linux/KVM. Cross-host restore mode rejects the capture host's ID.
func TestPhase1B(t *testing.T) {
	if os.Getenv("HYPERCALL_SNAPSHOT_KVM_TEST") != "1" {
		t.Skip("set HYPERCALL_SNAPSHOT_KVM_TEST=1 for real snapshot acceptance; a skip is not qualification")
	}
	required := func(name string) string {
		t.Helper()
		v := os.Getenv(name)
		if v == "" {
			t.Fatalf("opted-in snapshot qualification requires %s", name)
		}
		return v
	}
	mode := os.Getenv("HYPERCALL_SNAPSHOT_MODE")
	if mode == "" {
		mode = "local"
	}
	commands := map[string]string{"local": "snapshot-qualify", "capture": "snapshot-capture", "restore": "snapshot-restore"}
	command, ok := commands[mode]
	if !ok {
		t.Fatal("invalid HYPERCALL_SNAPSHOT_MODE")
	}
	args := []string{command, "--config", required("HYPERCALL_HOST_CONFIG"), "--state-dir", required("HYPERCALL_QUALIFICATION_STATE"), "--cgroup-dir", required("HYPERCALL_CGROUP_DIR")}
	if bucket := os.Getenv("HYPERCALL_GCS_BUCKET"); bucket != "" {
		if os.Getenv("HYPERCALL_SNAPSHOT_STORE") != "" {
			t.Fatal("select GCS or file storage, not both")
		}
		args = append(args, "--gcs-bucket", bucket, "--gcs-prefix", required("HYPERCALL_GCS_PREFIX"), "--gcs-token-file", required("HYPERCALL_GCS_TOKEN_FILE"))
	} else {
		args = append(args, "--snapshot-store", required("HYPERCALL_SNAPSHOT_STORE"))
	}
	if mode != "local" {
		args = append(args, "--handoff", required("HYPERCALL_SNAPSHOT_HANDOFF"))
	}
	if path := os.Getenv("HYPERCALL_QUALIFICATION_REPORT"); path != "" {
		args = append(args, "--output", path)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, required("HYPERCALL_TESTDRIVER"), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if len(out) > 8<<20 {
		t.Fatal("snapshot report exceeds8MiB")
	}
	t.Log(string(out))
	if err != nil {
		t.Fatalf("snapshot qualification failed: %v\n%s", err, stderr.String())
	}
	var r struct {
		SchemaVersion int    `json:"schema_version"`
		Profile       string `json:"profile"`
		Checks        []struct {
			ID     string `json:"id"`
			Passed bool   `json:"passed"`
			Error  string `json:"error"`
		} `json:"checks"`
	}
	if err = json.Unmarshal(out, &r); err != nil {
		t.Fatal(err)
	}
	profiles := map[string]string{"local": "phase1b-local", "capture": "phase1b-capture", "restore": "phase1b-restore"}
	if r.SchemaVersion != 1 || r.Profile != profiles[mode] {
		t.Fatalf("wrong snapshot report schema/profile: %d/%s", r.SchemaVersion, r.Profile)
	}
	seen := map[string]bool{}
	for _, c := range r.Checks {
		if !c.Passed {
			t.Errorf("%s: %s", c.ID, c.Error)
		}
		seen[c.ID] = c.Passed
	}
	checks := []string{"host-prerequisites", "cleanup", "log-redaction"}
	switch mode {
	case "local":
		checks = append(checks, "snapshot-boot", "checkpoint-capture", "capture-expiry", "capture-crash-recovery", "fork-continuity", "restored-clock", "restored-health-stability", "fork-isolation", "fork-authentication", "fork-retry", "artifact-rejection", "restore-crash-recovery")
	case "capture":
		checks = append(checks, "snapshot-boot", "checkpoint-capture", "cross-host-export")
	case "restore":
		checks = append(checks, "cross-host-source", "cross-host-restore", "restored-clock", "restored-health-stability", "restored-continuation", "restored-authentication")
	}
	for _, id := range checks {
		if !seen[id] {
			t.Errorf("required snapshot gate missing or failed: %s", id)
		}
	}
}
