//go:build !linux

package guest

import (
	"errors"
	"os/exec"
)

func validateWorkloadCgroup(path string, trusted bool) error {
	if path == "" && trusted {
		return nil
	}
	return errors.New("workload cgroups require Linux")
}

type execCgroup struct{}

func commandCgroup(_ string, _ *exec.Cmd) (*execCgroup, error) { return &execCgroup{}, nil }
func (*execCgroup) kill() error                                { return nil }
func (*execCgroup) close() error                               { return nil }
func preventPrivilegeGain() error                              { return nil }
