// hypercall-host-service is the privileged, mutually authenticated runtime
// bridge. Run separately from the unprivileged API/controller process.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/gduchidze/hypercall/internal/controlplane"
	"github.com/gduchidze/hypercall/internal/hostapi"
	"github.com/gduchidze/hypercall/internal/sandbox"
	"github.com/gduchidze/hypercall/internal/sandbox/hostdoctor"
	"github.com/gduchidze/hypercall/internal/snapshot"
)

func main() {
	if e := run(); e != nil {
		slog.Error("host service stopped", "error", e)
		os.Exit(1)
	}
}
func run() error {
	listen := flag.String("listen", "127.0.0.1:9444", "internal mTLS listen address")
	ca := flag.String("client-ca", "", "trusted controller CA PEM")
	cert := flag.String("tls-cert", "", "host server certificate PEM")
	key := flag.String("tls-key", "", "host private key PEM")
	principal := flag.String("controller-uri", "", "exact authorized controller certificate URI SAN")
	tenant := flag.String("tenant", "", "trusted tenant ID")
	project := flag.String("project", "", "trusted project ID")
	artifacts := flag.String("artifacts", "", "verified hostdoctor artifact configuration")
	state := flag.String("runtime-state", "", "private durable runtime state directory")
	ledger := flag.String("ledger-state", "", "separate private durable bridge ledger directory")
	cgroup := flag.String("cgroup-dir", "", "delegated cgroups v2 parent")
	uid := flag.Int("jailer-uid", -1, "non-root jailer/workload uid")
	gid := flag.Int("jailer-gid", -1, "non-root jailer/workload gid")
	controlled := flag.Bool("controlled-snapshots", false, "opt in to UNQUALIFIED controlled-Python snapshot implementation")
	localStore := flag.String("snapshot-store", "", "private local snapshot object directory")
	bucket := flag.String("gcs-bucket", "", "existing authorized snapshot GCS bucket")
	prefix := flag.String("gcs-prefix", "", "authorized GCS object prefix")
	tokenFile := flag.String("gcs-token-file", "", "0600 externally refreshed host OAuth token file")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	tlsConfig, e := hostapi.ServerTLS(*ca, *cert, *key)
	if e != nil {
		return e
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// sandbox.New independently verifies and stages every artifact before starting.
	snapshotPath, manifest, cleanup, e := snapshotConfig(*artifacts)
	if e != nil {
		return e
	}
	defer cleanup()
	var objects snapshot.Objects
	if *localStore != "" && *bucket != "" {
		return errors.New("choose local snapshot store or GCS, not both")
	}
	if *localStore != "" {
		objects, e = snapshot.NewFileObjects(*localStore)
	} else if *bucket != "" {
		objects, e = snapshot.NewGCSObjects(snapshot.GCSConfig{Bucket: *bucket, Prefix: *prefix, TokenSource: func(context.Context) (string, error) { return readToken(*tokenFile) }})
	}
	if e != nil {
		return e
	}
	var store *snapshot.Store
	if objects != nil {
		store, e = snapshot.New(objects, snapshot.Options{Logger: slog.Default()})
		if e != nil {
			return e
		}
	}
	if *controlled && store == nil {
		return errors.New("controlled snapshot opt-in requires an explicit snapshot store")
	}
	guestDigest, e := pinnedGuestDigest(manifest)
	if e != nil {
		return e
	}
	runtime, e := sandbox.New(ctx, sandbox.Config{Scope: sandbox.Scope{TenantID: *tenant, ProjectID: *project}, ArtifactConfigPath: snapshotPath, SnapshotStore: store, StateDir: *state, CgroupDir: *cgroup, JailerUID: *uid, JailerGID: *gid, Logger: slog.Default()})
	if e != nil {
		return e
	}
	defer runtime.Close()
	bridge, e := hostapi.NewBridge(hostapi.Config{Scope: controlplane.Scope{TenantID: *tenant, ProjectID: *project}, StateDir: *ledger, TemplateDigest: "sha256:" + strings.ToLower(manifest.Template.SHA256), Logger: slog.Default(), SnapshotStore: store, ControlledSnapshots: *controlled, ControlledAgentDigest: guestDigest}, runtime)
	if e != nil {
		return e
	}
	defer bridge.Close()
	server, e := hostapi.NewServer(bridge, tlsConfig, *principal)
	if e != nil {
		return e
	}
	listener, e := net.Listen("tcp", *listen)
	if e != nil {
		return fmt.Errorf("host listen: %w", e)
	}
	defer listener.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			server.Stop()
		case <-done:
		}
	}()
	slog.Info("host service ready", "scope_tenant", *tenant, "scope_project", *project, "listen", listener.Addr(), "protocol", "hypercall.host.v1", "template_digest", "sha256:"+manifest.Template.SHA256)
	e = server.Serve(listener)
	if ctx.Err() != nil {
		return nil
	}
	return e
}

// Read once, then give the runtime the same private bytes whose template digest
// configures authorization. Replacing the operator's original path cannot make
// the bridge authorize template A while the runtime boots template B.
func snapshotConfig(configPath string) (string, hostdoctor.ArtifactConfig, func(), error) {
	var c hostdoctor.ArtifactConfig
	f, e := os.OpenFile(configPath, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return "", c, nil, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return "", c, nil, e
	}
	if !st.Mode().IsRegular() || st.Size() > 1<<20 || st.Mode().Perm()&0022 != 0 {
		return "", c, nil, errors.New("runtime config must be a regular non-group-writable file <=1MiB")
	}
	data, e := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if e != nil {
		return "", c, nil, e
	}
	if len(data) > 1<<20 {
		return "", c, nil, errors.New("runtime config exceeded size limit")
	}
	if e = json.Unmarshal(data, &c); e != nil {
		return "", c, nil, e
	}
	dir, e := os.MkdirTemp("", "hypercall-host-config-")
	if e != nil {
		return "", c, nil, e
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	p := filepath.Join(dir, "artifacts.json")
	if e = os.WriteFile(p, data, 0400); e != nil {
		cleanup()
		return "", c, nil, e
	}
	return p, c, cleanup, nil
}

func pinnedGuestDigest(c hostdoctor.ArtifactConfig) (string, error) {
	f, e := os.OpenFile(c.Template.Path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return "", e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return "", e
	}
	if !st.Mode().IsRegular() || st.Size() > 1<<20 {
		return "", errors.New("invalid template manifest")
	}
	data, e := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if e != nil {
		return "", e
	}
	h := sha256.Sum256(data)
	if hex.EncodeToString(h[:]) != strings.ToLower(c.Template.SHA256) {
		return "", errors.New("template manifest digest mismatch")
	}
	var m hostdoctor.TemplateManifest
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if e = decoder.Decode(&m); e != nil {
		return "", e
	}
	return "sha256:" + strings.ToLower(m.GuestAgent.SHA256), nil
}
func readToken(p string) (string, error) {
	f, e := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return "", errors.New("host OAuth token unavailable")
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || st.Size() > 16384 {
		return "", errors.New("host OAuth token must be a private 0600 regular file")
	}
	data, e := io.ReadAll(io.LimitReader(f, 16385))
	if e != nil || len(data) > 16384 {
		return "", errors.New("host OAuth token unavailable")
	}
	return strings.TrimSpace(string(data)), nil
}
