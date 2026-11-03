//go:build linux

package guest

import (
	"net"

	"github.com/mdlayher/vsock"
)

// ListenVSock binds only the guest's AF_VSOCK transport, never TCP.
func ListenVSock(port uint32) (net.Listener, error) { return vsock.Listen(port, nil) }
