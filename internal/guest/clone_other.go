//go:build !linux

package guest

import "errors"

func readCloneSession() (*cloneSession, error) {
	return nil, errors.New("clone renewal requires Linux guest")
}
func reseedKernel([]byte) error { return errors.New("clone RNG renewal requires Linux guest") }

func syncGuestDisks() {}

func setGuestClock(int64) error { return errors.New("guest UTC refresh requires Linux") }
