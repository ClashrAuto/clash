package shadowsocks

import (
	"net"

	"github.com/metacubex/clashauto/adapter/inbound"
	N "github.com/metacubex/clashauto/common/net"
	"github.com/metacubex/clashauto/common/sockopt"
	C "github.com/metacubex/clashauto/constant"
	"github.com/metacubex/clashauto/log"
	"github.com/metacubex/clashauto/transport/shadowsocks/core"
	"github.com/metacubex/clashauto/transport/socks5"
)

type UDPListener struct {
	packetConn net.PacketConn
	closed     bool
}

func NewUDP(addr string, pickCipher core.Cipher, tunnel C.Tunnel, additions ...inbound.Addition) (*UDPListener, error) {
	l, err := inbound.ListenPacket("udp", addr)
	if err != nil {
		return nil, err
	}

	if err := sockopt.UDPReuseaddr(l); err != nil {
		log.Warnln("Failed to Reuse UDP Address: %s", err)
	}

	sl := &UDPListener{l, false}
	conn := pickCipher.PacketConn(N.NewEnhancePacketConn(l))
	go func() {
		for {
			data, put, remoteAddr, err := conn.WaitReadFrom()
			if err != nil {
				if put != nil {
					put()
				}
				if sl.closed {
					break
				}
				continue
			}
			handleSocksUDP(conn, tunnel, data, put, remoteAddr, additions...)
		}
	}()

	return sl, nil
}

func (l *UDPListener) Close() error {
	l.closed = true
	return l.packetConn.Close()
}

func (l *UDPListener) LocalAddr() net.Addr {
	return l.packetConn.LocalAddr()
}

func handleSocksUDP(pc net.PacketConn, tunnel C.Tunnel, buf []byte, put func(), addr net.Addr, additions ...inbound.Addition) {
	tgtAddr := socks5.SplitAddr(buf)
	if tgtAddr == nil {
		// Unresolved UDP packet, return buffer to the pool
		if put != nil {
			put()
		}
		return
	}
	target := tgtAddr
	payload := buf[len(tgtAddr):]

	packet := &packet{
		pc:      pc,
		rAddr:   addr,
		payload: payload,
		put:     put,
	}
	tunnel.HandleUDPPacket(inbound.NewPacket(target, packet, C.SHADOWSOCKS, additions...))
}
