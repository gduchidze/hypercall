// hypercall-testdriver is a privileged Phase 1A/1B acceptance operator.
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
	Profile       string    `json:"profile,omitempty"`
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
	required := requiredChecks
	if r.Profile != "" {
		required = snapshotChecks(r.Profile)
		if len(required) == 0 {
			return false
		}
	}
	for _, id := range required {
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
	SnapshotStore, Handoff                               string
	GCSBucket, GCSPrefix, GCSTokenFile                   string
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
		fmt.Fprintln(os.Stderr, "usage: hypercall-testdriver qualify|hold|snapshot-qualify|snapshot-capture|snapshot-restore --config PATH --state-dir PATH --cgroup-dir PATH [--snapshot-store PATH --handoff PATH]")
		return 2
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	var o Options
	flags.StringVar(&o.ConfigPath, "config", "", "pinned host artifact configuration")
	flags.StringVar(&o.StateDir, "state-dir", "", "private qualification state directory (dedicated to this run)")
	flags.StringVar(&o.CgroupDir, "cgroup-dir", "", "delegated cgroups v2 parent")
	flags.StringVar(&o.Python, "python", "python3", "host Python with locked grpcio and generated bindings")
	flags.StringVar(&o.PythonProbe, "python-probe", "", "absolute scripts/qualify-python.py path")
	flags.StringVar(&o.SnapshotStore, "snapshot-store", "", "private absolute snapshot object directory")
	flags.StringVar(&o.Handoff, "handoff", "", "private absolute cross-host handoff JSON path")
	flags.StringVar(&o.GCSBucket, "gcs-bucket", "", "operator-authorized GCS bucket; mutually exclusive with snapshot-store")
	flags.StringVar(&o.GCSPrefix, "gcs-prefix", "", "GCS object namespace")
	flags.StringVar(&o.GCSTokenFile, "gcs-token-file", "", "private absolute refreshed OAuth token file; host only")
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
	if args[0] != "qualify" && args[0] != "snapshot-qualify" && args[0] != "snapshot-capture" && args[0] != "snapshot-restore" && args[0] != "snapshot-hold" && args[0] != "snapshot-upload-hold" {
		fmt.Fprintln(os.Stderr, "unknown command")
		return 2
	}
	if strings.HasPrefix(args[0], "snapshot-") {
		if err := o.validateSnapshot(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		if args[0] != "snapshot-qualify" && !filepath.IsAbs(o.Handoff) {
			fmt.Fprintln(os.Stderr, "snapshot capture/restore/hold require absolute --handoff")
			return 2
		}
	}
	if args[0] == "snapshot-hold" || args[0] == "snapshot-upload-hold" {
		var err error
		if args[0] == "snapshot-upload-hold" {
			err = snapshotUploadHold(ctx, o)
		} else {
			err = snapshotHold(ctx, o)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	var r Report
	var secrets []string
	if args[0] == "qualify" {
		r, secrets = qualify(ctx, o)
	} else {
		r, secrets = qualifySnapshot(ctx, o, args[0])
	}
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

func (o Options) validateSnapshot() error {
	if o.GCSBucket != "" {
		if o.SnapshotStore != "" || o.GCSPrefix == "" || !filepath.IsAbs(o.GCSTokenFile) {
			return errors.New("GCS requires --gcs-prefix, absolute --gcs-token-file and no --snapshot-store")
		}
	} else if !filepath.IsAbs(o.SnapshotStore) || o.GCSPrefix != "" || o.GCSTokenFile != "" {
		return errors.New("select absolute --snapshot-store or --gcs-bucket with --gcs-token-file")
	}
	return nil
}
