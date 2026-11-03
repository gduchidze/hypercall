//go:build !linux

package sandbox

import (
	"context"
	"fmt"
)

type Backend struct{ SandboxBackend }

func New(_ context.Context, c Config) (*Backend, error) {
	if err := validateConfig(c); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: requires Linux KVM and cgroups v2", ErrUnavailable)
}
