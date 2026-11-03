//go:build linux || darwin

package hostdoctor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

func openRegular(path string) (*os.File, error) {
	// O_NONBLOCK keeps a FIFO's open from waiting for a writer. O_NOFOLLOW
	// rejects a symlink in the final path component. fstat checks the opened
	// object, so a rename between a path check and open cannot bypass its type.
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	stat, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !stat.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("%s must be a regular file", path)
	}
	return file, nil
}

func prepareExecutable(ctx context.Context, file *os.File, expectedSHA256 string) (string, []*os.File, func() error, error) {
	if runtime.GOOS == "linux" {
		// ExtraFiles installs the verified object as descriptor 3 without
		// close-on-exec. Interpreters also consume that object, rather than
		// resolving the configured artifact pathname a second time.
		return "/proc/self/fd/3", []*os.File{file}, func() error { return nil }, nil
	}
	// Darwin rejects execve(/dev/fd/N). Execute a private verified copy of the
	// open descriptor instead; never fall back to reopening the artifact path.
	dir, err := os.MkdirTemp("", "hypercall-verified-executable-")
	if err != nil {
		return "", nil, nil, err
	}
	cleanup := func() error { return os.RemoveAll(dir) }
	fail := func(err error) (string, []*os.File, func() error, error) {
		if cleanupErr := cleanup(); cleanupErr != nil {
			err = fmt.Errorf("%w; remove %s: %v", err, dir, cleanupErr)
		}
		return "", nil, nil, err
	}
	stat, err := os.Stat(dir)
	if err != nil {
		return fail(err)
	}
	owner, ok := stat.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) || stat.Mode().Perm() != 0700 {
		return fail(fmt.Errorf("private executable directory must be owned by effective UID %d with mode 0700", os.Geteuid()))
	}
	path := filepath.Join(dir, "executable")
	snapshot, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return fail(err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(snapshot, hash), contextReader{ctx, file})
	if copyErr == nil && !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), expectedSHA256) {
		copyErr = fmt.Errorf("artifact changed while copying verified executable")
	}
	if copyErr == nil {
		copyErr = snapshot.Chmod(0500)
	}
	closeErr := snapshot.Close()
	if copyErr != nil {
		return fail(copyErr)
	}
	if closeErr != nil {
		return fail(closeErr)
	}
	return path, nil, cleanup, nil
}
