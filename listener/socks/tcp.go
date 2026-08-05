package socks

import (
	"context"
	"errors"
	"io"
	"net"

	"github.com/ClashrAuto/coast/adapter/inbound"
	N "github.com/ClashrAuto/coast/common/net"
	"github.com/ClashrAuto/coast/component/auth"
	"github.com/ClashrAuto/coast/component/ca"
	"github.com/ClashrAuto/coast/component/ech"
	C "github.com/ClashrAuto/coast/constant"
	authStore "github.com/ClashrAuto/coast/listener/auth"
	LC "github.com/ClashrAuto/coast/listener/config"
	"github.com/ClashrAuto/coast/listener/reality"
	"github.com/ClashrAuto/coast/ntp"
	"github.com/ClashrAuto/coast/transport/socks4"
	"github.com/ClashrAuto/coast/transport/socks5"

	"github.com/metacubex/tls"
)

type Listener struct {
	listener net.Listener
	addr     string
	closed   bool
}

// RawAddress implements C.Listener
func (l *Listener) RawAddress() string {
	return l.addr
}

// Address implements C.Listener
func (l *Listener) Address() string {
	return l.listener.Addr().String()
}

// Close implements C.Listener
func (l *Listener) Close() error {
	l.closed = true
	return l.listener.Close()
}

func defaultConfig(addr string) LC.AuthServer {
	return LC.AuthServer{Enable: true, Listen: addr, AuthStore: authStore.Default}
}

func New(addr string, tunnel C.Tunnel, additions ...inbound.Addition) (*Listener, error) {
	return NewWithConfig(defaultConfig(addr), inbound.NewListenConfig(), tunnel, additions...)
}

func NewWithConfig(config LC.AuthServer, lc C.InboundListenConfig, tunnel C.Tunnel, additions ...inbound.Addition) (*Listener, error) {
	isDefault := false
	if len(additions) == 0 {
		isDefault = true
		additions = []inbound.Addition{
			inbound.WithInName("DEFAULT-SOCKS"),
			inbound.WithSpecialRules(""),
		}
	}

	l, err := lc.Listen(context.Background(), "tcp", config.Listen)
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{Time: ntp.Now}
	var realityBuilder *reality.Builder

	if config.Certificate != "" && config.PrivateKey != "" {
		certLoader, err := ca.NewTLSKeyPairLoader(config.Certificate, config.PrivateKey)
		if err != nil {
			return nil, err
		}
		tlsConfig.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return certLoader()
		}

		if config.EchKey != "" {
			err = ech.LoadECHKey(config.EchKey, tlsConfig)
			if err != nil {
				return nil, err
			}
		}
	}
	tlsConfig.ClientAuth = ca.ClientAuthTypeFromString(config.ClientAuthType)
	if len(config.ClientAuthCert) > 0 {
		if tlsConfig.ClientAuth == tls.NoClientCert {
			tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
		}
	}
	if tlsConfig.ClientAuth == tls.VerifyClientCertIfGiven || tlsConfig.ClientAuth == tls.RequireAndVerifyClientCert {
		pool, err := ca.LoadCertificates(config.ClientAuthCert)
		if err != nil {
			return nil, err
		}
		tlsConfig.ClientCAs = pool
	}
	if config.RealityConfig.PrivateKey != "" {
		if tlsConfig.GetCertificate != nil {
			return nil, errors.New("certificate is unavailable in reality")
		}
		if tlsConfig.ClientAuth != tls.NoClientCert {
			return nil, errors.New("client-auth is unavailable in reality")
		}
		realityBuilder, err = config.RealityConfig.Build(tunnel)
		if err != nil {
			return nil, err
		}
	}

	if realityBuilder != nil {
		l = realityBuilder.NewListener(l)
	} else if tlsConfig.GetCertificate != nil {
		l = tls.NewListener(l, tlsConfig)
	}

	sl := &Listener{
		listener: l,
		addr:     config.Listen,
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				if sl.closed {
					break
				}
				continue
			}
			store := config.AuthStore
			if isDefault || store == authStore.Default { // only apply on default listener
				if !inbound.IsRemoteAddrDisAllowed(c.RemoteAddr()) {
					_ = c.Close()
					continue
				}
				if inbound.SkipAuthRemoteAddr(c.RemoteAddr()) {
					store = authStore.Nil
				}
			}
			go handleSocks(c, tunnel, store, additions...)
		}
	}()

	return sl, nil
}

func handleSocks(conn net.Conn, tunnel C.Tunnel, store auth.AuthStore, additions ...inbound.Addition) {
	bufConn := N.NewBufferedConn(conn)
	head, err := bufConn.Peek(1)
	if err != nil {
		conn.Close()
		return
	}

	switch head[0] {
	case socks4.Version:
		HandleSocks4(bufConn, tunnel, store, additions...)
	case socks5.Version:
		HandleSocks5(bufConn, tunnel, store, additions...)
	default:
		conn.Close()
	}
}

func HandleSocks4(conn net.Conn, tunnel C.Tunnel, store auth.AuthStore, additions ...inbound.Addition) {
	authenticator := store.Authenticator()
	addr, _, user, err := socks4.ServerHandshake(conn, authenticator)
	if err != nil {
		conn.Close()
		return
	}
	additions = append(additions, inbound.WithInUser(user))
	tunnel.HandleTCPConn(inbound.NewSocket(socks5.ParseAddr(addr), conn, C.SOCKS4, additions...))
}

func HandleSocks5(conn net.Conn, tunnel C.Tunnel, store auth.AuthStore, additions ...inbound.Addition) {
	authenticator := store.Authenticator()
	target, command, user, err := socks5.ServerHandshake(conn, authenticator)
	if err != nil {
		conn.Close()
		return
	}
	if command == socks5.CmdUDPAssociate {
		defer conn.Close()
		// ★ 认证得到的 user 以前在这里被直接丢掉，于是这条会话后续的 UDP 数据报在核心眼里
		//   完全没有身份（IN-USER 不命中、inboundUser 恒空）。改成按客户端在 ASSOCIATE 请求里
		//   声明的 UDP 源地址登记，寿命跟着这条控制连接 —— 下面的 io.Copy 一返回就注销。
		//   target 就是那个请求的 DST.ADDR:DST.PORT（见 socks5.ServerHandshake）。
		//   客户端没声明（0.0.0.0:0）时 udpAssociateKey 返回空串，登记是空操作，行为不变。
		defer registerUDPUser(udpAssociateKey(target), user)()
		io.Copy(io.Discard, conn)
		return
	}
	additions = append(additions, inbound.WithInUser(user))
	tunnel.HandleTCPConn(inbound.NewSocket(target, conn, C.SOCKS5, additions...))
}

// udpAssociateKey 把 UDP ASSOCIATE 请求里声明的来源地址转成 udpUsers 的键，
// 形态与 net.PacketConn 读到的 addr.String() 一致（v4 `1.2.3.4:p`，v6 `[::1]:p`）。
//
// 端口为 0 视为「没声明」：RFC 1928 允许客户端在还不知道自己端口时填 0，那样的声明
// 无法唯一定位一条会话，登记了反而可能张冠李戴 —— 宁可不认。域名型 ATYP 同理
// （UDPAddr() 对它返回 nil）：来源地址必须是核心能逐包比对的字面地址。
func udpAssociateKey(target socks5.Addr) string {
	ua := target.UDPAddr()
	if ua == nil || ua.Port == 0 || ua.IP == nil || ua.IP.IsUnspecified() {
		return ""
	}
	return ua.String()
}
