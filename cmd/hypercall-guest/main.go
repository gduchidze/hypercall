package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	harnessv1 "github.com/gduchidze/hypercall/gen/go/hypercall/harness/v1"
	"github.com/gduchidze/hypercall/internal/guest"
	"google.golang.org/grpc"
)

type sessionConfig struct {
	HarnessSession *harnessv1.HarnessSession   `json:"harness_session,omitempty"`
	Identity       *guestv1.AllocationIdentity `json:"identity"`
	Credential     string                      `json:"credential"`
	ExpiresAt      time.Time                   `json:"expires_at"`
	Entropy        string                      `json:"entropy"`
}

func main() {
	guest.RunExecHelper()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("guest stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	workspace := flag.String("workspace", "/workspace", "absolute guest workspace (must already exist)")
	configPath := flag.String("session-config", "", "0600 out-of-band allocation/credential/expiry JSON file")
	testSocket := flag.String("test-unix-socket", "", "TRUSTED TESTS ONLY: same-user commands over this Unix socket")
	port := flag.Uint("vsock-port", 1024, "production guest vsock port")
	uid := flag.Int("uid", -1, "production workload uid (must be non-root)")
	gid := flag.Int("gid", -1, "production workload gid (must be non-root)")
	allowedEnv := flag.String("allow-env", "LANG,LC_ALL,TZ,PYTHONUNBUFFERED", "comma-separated explicitly allowed environment keys")
	harnessSocket := flag.String("harness-socket", "", "private workload-owned local harness Unix socket")
	harnessProgram := flag.String("harness-program", "/opt/hypercall/agent.py", "verified rootfs cooperative Python entrypoint")
	workloadCgroup := flag.String("workload-cgroup", "", "production cgroup v2 subtree with finite workload limits")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	session, err := loadSession(*configPath)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return errors.New("resolve guest executable failed")
	}
	var allowed []string
	if *allowedEnv != "" {
		allowed = strings.Split(*allowedEnv, ",")
	}
	svc, err := guest.New(guest.Config{HarnessSession: session.HarnessSession, HarnessSocket: *harnessSocket, HarnessProgram: *harnessProgram, Workspace: *workspace, Identity: session.Identity, Credential: session.Credential, ExpiresAt: session.ExpiresAt, TrustedLocal: *testSocket != "", ExecHelper: executable, UID: *uid, GID: *gid, AllowedEnv: allowed, WorkloadCgroup: *workloadCgroup, Logger: logger})
	if err != nil {
		return err
	}
	defer svc.Close()
	var lis net.Listener
	if *testSocket != "" {
		// ListenUnix refuses an existing path; never remove another server's socket.
		lis, err = net.ListenUnix("unix", &net.UnixAddr{Name: *testSocket, Net: "unix"})
		if err == nil {
			err = os.Chmod(*testSocket, 0o600)
		}
	} else {
		if *port == 0 || uint64(*port) > 1<<32-1 {
			return errors.New("invalid vsock port")
		}
		lis, err = guest.ListenVSock(uint32(*port))
	}
	if lis != nil {
		defer lis.Close()
	}
	if err != nil {
		return errors.New("guest listener failed")
	}
	srv := grpc.NewServer(grpc.MaxRecvMsgSize(guest.MaxFileBytes+(64<<10)), grpc.MaxHeaderListSize(8192), grpc.MaxConcurrentStreams(32), grpc.WaitForHandlers(true))
	guestv1.RegisterGuestServer(srv, svc)
	var local *grpc.Server
	if *harnessSocket != "" {
		localListener, e := guest.ListenHarness(*harnessSocket, *uid, *gid)
		if e != nil {
			return e
		}
		defer localListener.Close()
		local = grpc.NewServer(grpc.MaxRecvMsgSize(128<<10), grpc.MaxSendMsgSize(128<<10), grpc.MaxConcurrentStreams(8), grpc.MaxHeaderListSize(8192))
		harnessv1.RegisterHarnessServer(local, svc)
		defer local.Stop()
		go func() {
			if e := local.Serve(localListener); e != nil {
				logger.Error("local harness listener stopped")
				srv.Stop()
			}
		}()
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-signals:
			if local != nil {
				local.Stop()
			}
			srv.Stop()
		case <-done:
		}
	}()
	logger.Info("guest ready", "transport", map[bool]string{true: "trusted-test-unix", false: "vsock"}[*testSocket != ""], "epoch", session.Identity.Epoch)
	return srv.Serve(lis)
}

func loadSession(path string) (*sessionConfig, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("open session config failed")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > 16384 {
		return nil, errors.New("session config must be a regular 0600 file <=16KiB")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 16385))
	decoder.DisallowUnknownFields()
	var session sessionConfig
	if err := decoder.Decode(&session); err != nil {
		return nil, errors.New("invalid session config JSON")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("session config must contain one JSON object")
	}
	return &session, nil
}
