package sandbox_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gduchidze/hypercall/internal/sandbox"
)

func TestVSockHandshakePreservesFirstRPCBytes(t *testing.T) {
	path := socketPath(t)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := make(chan error, 1)
	go func() {
		c, e := listener.Accept()
		if e != nil {
			server <- e
			return
		}
		defer c.Close()
		line, e := bufio.NewReader(c).ReadString('\n')
		if e == nil && line != "CONNECT 1024\n" {
			e = io.ErrUnexpectedEOF
		}
		if e == nil {
			_, e = c.Write([]byte("OK 1073741824\nRPC"))
		}
		server <- e
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := sandbox.DialVSock(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	data := make([]byte, 3)
	if _, err = io.ReadFull(conn, data); err != nil {
		t.Fatal(err)
	}
	if string(data) != "RPC" {
		t.Fatalf("lost RPC prefix: %q", data)
	}
	if err = <-server; err != nil {
		t.Fatal(err)
	}
}

func TestVSockRejectsInvalidAndOversizedHandshake(t *testing.T) {
	for _, response := range []string{"ERR 1\n", "OK 0\n", "OK 4294967296\n", "OK x\n", "OK 1 extra\n", string(make([]byte, 65))} {
		t.Run(response[:2], func(t *testing.T) {
			path := socketPath(t)
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			go func() {
				c, e := listener.Accept()
				if e != nil {
					return
				}
				defer c.Close()
				_, _ = bufio.NewReader(c).ReadString('\n')
				_, _ = c.Write([]byte(response))
			}()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			conn, err := sandbox.DialVSock(ctx, path)
			if conn != nil {
				conn.Close()
			}
			if err == nil {
				t.Fatal("invalid handshake accepted")
			}
		})
	}
}

func TestVSockHandshakeHonorsCancellation(t *testing.T) {
	path := socketPath(t)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		c, e := listener.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		<-done
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	conn, err := sandbox.DialVSock(ctx, path)
	if conn != nil {
		conn.Close()
	}
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("handshake ignored cancellation: %v", err)
	}
}

func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hc-vsock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "v.sock")
}
