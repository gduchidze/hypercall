package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// DialVSock connects through the Firecracker host UDS and completes its
// CONNECT 1024 handshake. RPC authentication remains mandatory after dialing.
// This transport does not grant authority or retrieve allocation credentials.
func DialVSock(ctx context.Context, path string) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	if d, has := ctx.Deadline(); has && d.Before(deadline) {
		deadline = d
	}
	if err = conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	if _, err = conn.Write([]byte("CONNECT 1024\n")); err != nil {
		return nil, err
	}
	// Read one byte at a time: never consume bytes belonging to gRPC/HTTP2.
	var line []byte
	var one [1]byte
	for len(line) < 64 {
		if _, err = conn.Read(one[:]); err != nil {
			return nil, err
		}
		line = append(line, one[0])
		if one[0] == '\n' {
			break
		}
	}
	response := string(line)
	if !strings.HasSuffix(response, "\n") || !strings.HasPrefix(response, "OK ") {
		return nil, errors.New("Firecracker vsock handshake rejected")
	}
	port, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(response, "OK "), "\n"), 10, 32)
	if err != nil || port == 0 {
		return nil, errors.New("invalid Firecracker vsock handshake")
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if !stop() {
		return nil, fmt.Errorf("vsock handshake canceled: %w", ctx.Err())
	}
	if err = conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	ok = true
	return conn, nil
}
