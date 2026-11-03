package snapshot

import (
	"context"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// The qualified profile has at most 8 GiB RAM and a 1 GiB workspace. A generous
// fixed upper bound also rejects unbounded special sources or accidental inputs.
const maxSourceBytes int64 = 16 << 30

func openSource(ctx context.Context, path string) (*os.File, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	// O_NONBLOCK rejects a FIFO without waiting for a writer; O_NOFOLLOW prevents
	// following a final-component symlink. Descriptor stat, not pathname stat,
	// establishes the file type. Keep this descriptor through hash and upload.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: source open rejected", ErrInvalid)
	}
	f := os.NewFile(uintptr(fd), "snapshot-source")
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maxSourceBytes {
		f.Close()
		return nil, 0, fmt.Errorf("%w: source must be a nonempty regular file within size bound", ErrInvalid)
	}
	return f, info.Size(), nil
}
