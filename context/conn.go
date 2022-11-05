package context

import (
	"net"

	CN "github.com/ClashrAuto/clash/common/net"
	C "github.com/ClashrAuto/clash/constant"
	"github.com/gofrs/uuid"
)

type ConnContext struct {
	id       uuid.UUID
	metadata *C.Metadata
	conn     net.Conn
}

func NewConnContext(conn net.Conn, metadata *C.Metadata) *ConnContext {
	id, _ := uuid.NewV4()

	return &ConnContext{
		id:       id,
		metadata: metadata,
		conn:     N.NewBufferedConn(conn),
	}
}

// ID implement C.ConnContext ID
func (c *ConnContext) ID() uuid.UUID {
	return c.id
}

// Metadata implement C.ConnContext Metadata
func (c *ConnContext) Metadata() *C.Metadata {
	return c.metadata
}

// Conn implement C.ConnContext Conn
func (c *ConnContext) Conn() net.Conn {
	return c.conn
}
