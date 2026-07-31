package dialer

import (
	"context"
	"net"
	"syscall"

	"github.com/ClashrAuto/coast/common/sockopt"
)

func addrReuseToListenConfig(lc *net.ListenConfig) {
	addControlToListenConfig(lc, func(ctx context.Context, network, address string, c syscall.RawConn) error {
		return sockopt.RawConnReuseaddr(c)
	})
}
