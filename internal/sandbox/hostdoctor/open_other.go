//go:build !linux && !darwin

package hostdoctor

import (
	"context"
	"errors"
	"os"
)

func openRegular(_ string) (*os.File, error) {
	return nil, errors.New("artifact inspection requires Linux or macOS nonblocking no-follow file operations")
}

func prepareExecutable(_ context.Context, _ *os.File, _ string) (string, []*os.File, func() error, error) {
	return "", nil, nil, errors.New("verified artifact execution requires Linux or macOS")
}
