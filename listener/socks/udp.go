package socks

import (
	"context"
	"net"

	"github.com/ClashrAuto/coast/adapter/inbound"
	N "github.com/ClashrAuto/coast/common/net"
	"github.com/ClashrAuto/coast/common/sockopt"
	C "github.com/ClashrAuto/coast/constant"
	LC "github.com/ClashrAuto/coast/listener/config"
	"github.com/ClashrAuto/coast/log"
	"github.com/ClashrAuto/coast/transport/socks5"
)

type UDPListener struct {
	packetConn net.PacketConn
	addr       string
	closed     bool
}

// RawAddress implements C.Listener
func (l *UDPListener) RawAddress() string {
	return l.addr
}

// Address implements C.Listener
func (l *UDPListener) Address() string {
	return l.packetConn.LocalAddr().String()
}

// Close implements C.Listener
func (l *UDPListener) Close() error {
	l.closed = true
	return l.packetConn.Close()
}

func NewUDP(addr string, tunnel C.Tunnel, additions ...inbound.Addition) (*UDPListener, error) {
	return NewUDPWithConfig(defaultConfig(addr), inbound.NewListenConfig(), tunnel, additions...)
}

func NewUDPWithConfig(config LC.AuthServer, lc C.InboundListenConfig, tunnel C.Tunnel, additions ...inbound.Addition) (*UDPListener, error) {
	if len(additions) == 0 {
		additions = []inbound.Addition{
			inbound.WithInName("DEFAULT-SOCKS"),
			inbound.WithSpecialRules(""),
		}
	}
	l, err := lc.ListenPacket(context.Background(), "udp", config.Listen)
	if err != nil {
		return nil, err
	}

	if err := sockopt.UDPReuseaddr(l); err != nil {
		log.Warnln("Failed to Reuse UDP Address: %s", err)
	}

	sl := &UDPListener{
		packetConn: l,
		addr:       config.Listen,
	}
	conn := N.NewEnhancePacketConn(l)
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
			handleSocksUDP(l, tunnel, data, put, remoteAddr, additions...)
		}
	}()

	return sl, nil
}

func handleSocksUDP(pc net.PacketConn, tunnel C.Tunnel, buf []byte, put func(), addr net.Addr, additions ...inbound.Addition) {
	target, payload, err := socks5.DecodeUDPPacket(buf)
	if err != nil {
		// Unresolved UDP packet, return buffer to the pool
		if put != nil {
			put()
		}
		return
	}
	packet := &packet{
		pc:      pc,
		rAddr:   addr,
		payload: payload,
		put:     put,
	}
	// ★ 逐包补上身份：UDP 中继是共享套接字，数据报本身不带认证信息，只能按**源地址**反查
	//   当初做过认证的那条控制连接（登记见 tcp.go 的 CmdUDPAssociate 分支 / udpuser.go）。
	//   查不到就保持原样 —— 免认证入站、以及没声明来源地址的第三方客户端都走这条路。
	if user := lookupUDPUser(addr.String()); user != "" {
		// ★ 必须**复制**再 append：additions 是监听器共享的那一份，TCP 那边的每条连接也在
		//   往它上面 append（tcp.go 的 WithInUser）。cap 有余量时直接 append 会写进同一块
		//   底层数组，把别人的身份覆盖掉 —— 这种串号只在并发下偶发，最难查。
		additions = append(append(make([]inbound.Addition, 0, len(additions)+1), additions...),
			inbound.WithInUser(user))
	}
	tunnel.HandleUDPPacket(inbound.NewPacket(target, packet, C.SOCKS5, additions...))
}
