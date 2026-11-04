package guest

import (
	"errors"
	"net"
	"os"
	"path/filepath"
)

// ListenHarness exposes only the local workload protocol. Its parent must be a
// trusted private directory; a tenant never receives a host-to-guest credential.
func ListenHarness(socket string, uid, gid int) (net.Listener, error) {
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket || len(socket) > 100 {
		return nil, errors.New("absolute bounded harness socket path required")
	}
	parent := filepath.Dir(socket)
	resolved, e := filepath.EvalSymlinks(parent)
	if e != nil || resolved != parent {
		return nil, errors.New("harness socket parent must not contain symlinks")
	}
	st, e := os.Stat(parent)
	if e != nil {
		return nil, e
	}
	if !st.IsDir() || st.Mode().Perm()&0007 != 0 || st.Mode().Perm()&0020 != 0 {
		return nil, errors.New("harness socket parent must restrict untrusted writers and other users")
	}
	listener, e := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if e != nil {
		return nil, e
	}
	ok := false
	defer func() {
		if !ok {
			listener.Close()
		}
	}()
	if e = os.Chmod(socket, 0600); e != nil {
		return nil, e
	}
	if uid >= 0 && gid >= 0 {
		if e = os.Chown(socket, uid, gid); e != nil {
			return nil, e
		}
	}
	ok = true
	return listener, nil
}
