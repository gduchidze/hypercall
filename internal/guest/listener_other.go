//go:build !linux

package guest

import (
	"errors"
	"net"
)

func ListenVSock(uint32) (net.Listener, error) {
	return nil, errors.New("vsock guest listener requires Linux")
}
