// hypercall-server composes the unprivileged API and durable controller. VM work
// is delegated to a separate authenticated host service, never embedded here.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gduchidze/hypercall/internal/api"
	cp "github.com/gduchidze/hypercall/internal/controlplane"
	"github.com/gduchidze/hypercall/internal/hostapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	if e := run(); e != nil {
		slog.Error("control plane stopped", "error", e)
		os.Exit(1)
	}
}
func loopback(address string) bool {
	host, _, e := net.SplitHostPort(address)
	if e != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
func privateFile(name string) ([]byte, error) {
	info, e := os.Stat(name)
	if e != nil {
		return nil, e
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("%s must be a private regular file (0600)", name)
	}
	return os.ReadFile(name)
}
func run() error {
	grpcAddress := flag.String("grpc-listen", "127.0.0.1:8081", "public gRPC listen address")
	httpAddress := flag.String("http-listen", "127.0.0.1:8080", "REST listen address")
	keysFile := flag.String("keys-file", "", "private JSON map of API keys to tenant/project grants")
	certFile := flag.String("tls-cert", "", "PEM server certificate chain")
	keyFile := flag.String("tls-key", "", "private PEM server key")
	serverName := flag.String("tls-server-name", "localhost", "certificate name used by the local REST bridge")
	insecureLocal := flag.Bool("insecure-loopback", false, "allow plaintext only on literal loopback listen addresses")
	hostCooperative := flag.Bool("host-cooperative-agents", false, "enable cooperative Python step/checkpoint/fork profile on a configured capable host")
	hostControlled := flag.Bool("host-controlled-snapshots", false, "enable controlled_python_v1 snapshot requests to an explicitly configured capable host")
	hostAddress := flag.String("host-address", "", "separate authenticated sandbox host gRPC endpoint")
	hostCA := flag.String("host-ca", "", "CA PEM for the sandbox host")
	hostCert := flag.String("host-cert", "", "controller client certificate PEM (URI SAN authorized by host)")
	hostKey := flag.String("host-key", "", "private controller client key PEM")
	hostName := flag.String("host-server-name", "", "verified sandbox host certificate DNS name")
	migrateOnly := flag.Bool("migrate-only", false, "apply checked migrations and exit")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dsn := os.Getenv("HYPERCALL_DATABASE_URL")
	if dsn == "" {
		return errors.New("HYPERCALL_DATABASE_URL is required")
	}
	startup, cancel := context.WithTimeout(ctx, 15*time.Second)
	store, e := cp.Open(startup, dsn)
	if e != nil {
		cancel()
		return errors.New("database connection failed")
	}
	defer store.Close()
	e = store.Migrate(startup)
	cancel()
	if e != nil {
		return fmt.Errorf("database migration: %w", e)
	}
	if *migrateOnly {
		return nil
	}
	raw, e := privateFile(*keysFile)
	if e != nil {
		return e
	}
	var keys map[string]api.Principal
	if e = json.Unmarshal(raw, &keys); e != nil {
		return errors.New("invalid scoped API key JSON")
	}
	auth, e := api.NewAuthenticator(keys)
	if e != nil {
		return e
	}
	var serverOptions []grpc.ServerOption
	var dialOption grpc.DialOption
	var tlsConfig *tls.Config
	if *certFile != "" || *keyFile != "" {
		if _, e = privateFile(*keyFile); e != nil {
			return e
		}
		cert, e := tls.LoadX509KeyPair(*certFile, *keyFile)
		if e != nil {
			return errors.New("invalid server TLS certificate/key")
		}
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}}
		serverOptions = append(serverOptions, grpc.Creds(credentials.NewTLS(tlsConfig)))
		pem, e := os.ReadFile(*certFile)
		if e != nil {
			return e
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return errors.New("invalid server certificate PEM")
		}
		dialOption = grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: *serverName}))
	} else {
		if !*insecureLocal || !loopback(*grpcAddress) || !loopback(*httpAddress) {
			return errors.New("TLS is required; plaintext needs --insecure-loopback and literal loopback addresses")
		}
		dialOption = grpc.WithTransportCredentials(insecure.NewCredentials())
	}
	backend := cp.Backend(cp.UnavailableBackend{})
	backendName := "unavailable"
	if *hostAddress != "" {
		if _, e = privateFile(*hostKey); e != nil {
			return e
		}
		hostTLS, e := hostapi.ClientTLS(*hostCA, *hostCert, *hostKey, *hostName)
		if e != nil {
			return errors.New("host endpoint requires valid CA, client certificate/key and server name")
		}
		hostConn, e := grpc.NewClient(*hostAddress, grpc.WithTransportCredentials(credentials.NewTLS(hostTLS)))
		if e != nil {
			return e
		}
		defer hostConn.Close()
		backend = hostapi.NewClient(hostConn, hostapi.ClientOptions{ControlledSnapshots: *hostControlled, CooperativeAgents: *hostCooperative})
		backendName = "authenticated-host"
	} else if *hostCA != "" || *hostCert != "" || *hostKey != "" || *hostName != "" || *hostControlled || *hostCooperative {
		return errors.New("host TLS flags require --host-address")
	}
	service := cp.NewService(store, backend)
	server := api.NewServer(service, auth, serverOptions...)
	listener, e := net.Listen("tcp", *grpcAddress)
	if e != nil {
		return e
	}
	defer listener.Close()
	host, port, e := net.SplitHostPort(listener.Addr().String())
	if e != nil {
		return e
	}
	if host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	conn, e := grpc.NewClient(net.JoinHostPort(host, port), dialOption)
	if e != nil {
		return e
	}
	defer conn.Close()
	httpServer := &http.Server{Addr: *httpAddress, Handler: server.HTTP(conn), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, TLSConfig: tlsConfig, MaxHeaderBytes: 16 << 10}
	errs := make(chan error, 3)
	go func() { errs <- server.GRPC().Serve(listener) }()
	go func() {
		if tlsConfig != nil {
			errs <- httpServer.ListenAndServeTLS(*certFile, *keyFile)
		} else {
			errs <- httpServer.ListenAndServe()
		}
	}()
	controller := &cp.Controller{Store: store, Backend: backend, WorkerID: fmt.Sprintf("controller-%d-%d", os.Getpid(), time.Now().UnixNano()), Logger: slog.Default()}
	go func() { errs <- controller.Run(ctx) }()
	slog.Info("control plane ready", "grpc", listener.Addr().String(), "rest", *httpAddress, "sandbox_backend", backendName)
	var result error
	select {
	case <-ctx.Done():
	case result = <-errs:
		stop()
	}
	shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	_ = httpServer.Shutdown(shutdown)
	graceful := make(chan struct{})
	go func() { server.GRPC().GracefulStop(); close(graceful) }()
	select {
	case <-graceful:
	case <-shutdown.Done():
		server.GRPC().Stop()
	}
	if errors.Is(result, http.ErrServerClosed) || errors.Is(result, context.Canceled) {
		return nil
	}
	return result
}
