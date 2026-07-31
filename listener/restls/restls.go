package restls

import (
	"context"
	"net"
	"runtime/debug"

	N "github.com/ClashrAuto/coast/common/net"
	C "github.com/ClashrAuto/coast/constant"
	LC "github.com/ClashrAuto/coast/listener/config"
	"github.com/ClashrAuto/coast/listener/inner"
	"github.com/ClashrAuto/coast/log"
	"github.com/ClashrAuto/coast/transport/restls"
)

type Builder struct {
	config *restls.ServerConfig
}

func New(config LC.ResTLS, tunnel C.Tunnel) *Builder {
	return &Builder{config: &restls.ServerConfig{
		ServerHostname: config.Dest,
		Password:       config.Password,
		RestlsScript:   config.RestlsScript,
		MinRecordLen:   config.MinRecordLen,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return inner.HandleTcp(tunnel, address, config.Proxy)
		},
	}}
}

func (b Builder) NewListener(listener net.Listener) net.Listener {
	return N.NewHandleContextListener(context.Background(), listener, func(ctx context.Context, conn net.Conn) (net.Conn, error) {
		return restls.Server(ctx, conn, b.config)
	}, func(a any) {
		stack := debug.Stack()
		log.Errorln("restls server panic: %s\n%s", a, stack)
	})
}
