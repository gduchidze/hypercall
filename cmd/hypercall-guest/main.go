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
	"github.com/gduchidze/hypercall/internal/guest"
	"google.golang.org/grpc"
)

type sessionConfig struct {
	Identity   *guestv1.AllocationIdentity `json:"identity"`
	Credential string                      `json:"credential"`
	ExpiresAt  time.Time                   `json:"expires_at"`
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
	svc, err := guest.New(guest.Config{Workspace: *workspace, Identity: session.Identity, Credential: session.Credential, ExpiresAt: session.ExpiresAt, TrustedLocal: *testSocket != "", ExecHelper: executable, UID: *uid, GID: *gid, AllowedEnv: allowed, Logger: logger})
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
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-signals:
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
