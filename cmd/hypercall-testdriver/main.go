// hypercall-testdriver is a privileged, local Phase 1A acceptance operator.
// It is not a public control plane. A successful report requires every real
// KVM gate, including fault injection and cleanup; no mock backend is accepted.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

var requiredChecks = []string{"host-prerequisites", "boot", "commands", "files", "python-vsock", "authentication", "scope-epoch", "concurrent-ttl", "cancellation", "deadline", "disconnect", "resource-limits", "deny-network", "vm-isolation", "guest-failure", "host-crash-recovery", "artifact-failures", "cleanup", "log-redaction"}

type Check struct {
	ID         string         `json:"id"`
	Passed     bool           `json:"passed"`
	DurationMS int64          `json:"duration_ms"`
	Error      string         `json:"error,omitempty"`
	Evidence   map[string]any `json:"evidence,omitempty"`
}
type Report struct {
	SchemaVersion int       `json:"schema_version"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at"`
	OS            string    `json:"os"`
	Architecture  string    `json:"architecture"`
	BinarySHA256  string    `json:"testdriver_sha256,omitempty"`
	Checks        []Check   `json:"checks"`
}

func (r Report) Passed() bool {
	seen := map[string]bool{}
	for _, c := range r.Checks {
		if !c.Passed {
			return false
		}
		seen[c.ID] = true
	}
	for _, id := range requiredChecks {
		if !seen[id] {
			return false
		}
	}
	return true
}
func marshalReport(r Report, secrets []string) ([]byte, error) {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	s := string(data)
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[REDACTED]")
		}
	}
	return []byte(s + "\n"), nil
}

type Options struct {
	ConfigPath, StateDir, CgroupDir, Python, PythonProbe string
	UID, GID                                             int
}

func (o Options) validate() error {
	for _, path := range []string{o.ConfigPath, o.StateDir, o.CgroupDir} {
		if !filepath.IsAbs(path) {
			return errors.New("explicit absolute config, state-dir and cgroup-dir are required")
		}
	}
	if o.UID <= 0 || o.GID <= 0 {
		return errors.New("non-root jailer uid and gid required")
	}
	return nil
}
func main() { os.Exit(run(os.Args[1:])) }
func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: hypercall-testdriver qualify|hold --config PATH --state-dir PATH --cgroup-dir PATH [--python python3 --python-probe PATH]")
		return 2
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	var o Options
	flags.StringVar(&o.ConfigPath, "config", "", "pinned host artifact configuration")
	flags.StringVar(&o.StateDir, "state-dir", "", "private qualification state directory (dedicated to this run)")
	flags.StringVar(&o.CgroupDir, "cgroup-dir", "", "delegated cgroups v2 parent")
	flags.StringVar(&o.Python, "python", "python3", "host Python with locked grpcio and generated bindings")
	flags.StringVar(&o.PythonProbe, "python-probe", "", "absolute scripts/qualify-python.py path")
	flags.IntVar(&o.UID, "jailer-uid", 65534, "non-root jailer uid")
	flags.IntVar(&o.GID, "jailer-gid", 65534, "non-root jailer gid")
	output := flags.String("output", "", "optional report JSON file (0600)")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		return 2
	}
	if err := o.validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if args[0] == "hold" {
		if err := hold(ctx, o); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	if args[0] != "qualify" {
		fmt.Fprintln(os.Stderr, "unknown command")
		return 2
	}
	r, secrets := qualify(ctx, o)
	data, err := marshalReport(r, secrets)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *output != "" {
		if err := os.WriteFile(*output, data, 0600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	if _, err = os.Stdout.Write(data); err != nil {
		return 1
	}
	if !r.Passed() {
		return 1
	}
	return 0
}
func newReport() Report {
	r := Report{SchemaVersion: 1, StartedAt: time.Now().UTC(), OS: runtime.GOOS, Architecture: runtime.GOARCH}
	if exe, err := os.Executable(); err == nil {
		if data, err := os.ReadFile(exe); err == nil {
			sum := sha256.Sum256(data)
			r.BinarySHA256 = hex.EncodeToString(sum[:])
		}
	}
	return r
}
