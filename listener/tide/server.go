package tide

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"os"
	"strings"

	"github.com/ClashrAuto/coast/adapter/inbound"
	C "github.com/ClashrAuto/coast/constant"
	LC "github.com/ClashrAuto/coast/listener/config"
	"github.com/ClashrAuto/coast/transport/socks5"

	tideproto "github.com/ClashrAuto/tide"
)

// TIDE 入站。和出站一样是**薄适配**：协议实现全在独立 module github.com/ClashrAuto/tide 里，
// 这里只负责把 TIDE 的流/数据报接进 clash 的 tunnel。
//
// ⚠️ 这里刻意用**标准库 crypto/tls**，而不是仓库里其它 listener 常用的 metacubex/tls。
// 原因是 TIDE 的信道绑定（spec §5）要从外层 TLS 取 Exporter（RFC 5705），
// 而整个协议的 MITM 检测与 bare 模式的安全前提都建立在那个导出值上。
// 换一套 TLS 实现就得同时保证它的导出器与标准库逐字节一致，收益为零、风险很大。

type Listener struct {
	closed    bool
	config    LC.TideServer
	srv       *tideproto.Server
	listeners []net.Listener
}

func New(config LC.TideServer, lc C.InboundListenConfig, tunnel C.Tunnel, additions ...inbound.Addition) (*Listener, error) {
	if len(additions) == 0 {
		additions = []inbound.Addition{
			inbound.WithInName("DEFAULT-TIDE"),
			inbound.WithSpecialRules(""),
		}
	}
	if config.PrivateKey == "" {
		return nil, errors.New("tide: private-key is required " +
			"(generate a pair with: tide-selftest -mode keygen)")
	}
	priv, err := tideproto.ParsePrivateKey(config.PrivateKey)
	if err != nil {
		return nil, err
	}
	if config.Cover == "" {
		// 不给掩护站点就等于放弃 spec §7 的抗主动探测。允许，但必须是**显式**选择——
		// 静默降级掉的安全属性没人会发现它已经没了。
		return nil, errors.New("tide: cover is required; point it at a real, reachable origin " +
			"(same host or same datacenter), or set cover: drop to knowingly accept probe exposure")
	}
	cert, err := loadKeyPair(config.Certificate, config.PrivateKeyPEM)
	if err != nil {
		return nil, err
	}

	// ★ 空用户表 = 谁都能用这台代理。
	//
	// 客户端在 TIDE 握手里**从不证明**自己知道私钥——它只需要**公钥**（做 KEM 封装），
	// 而公钥本来就要发给每一个客户端。所以没有 users: 的配置起出来的是一台
	// 开放中继，而且日志里一个字都没有。这与上面 cover 那条是同一类：
	// 静默降级掉的安全属性没人会发现它已经没了，所以必须**显式**选择。
	//
	// 库那边（tideproto.NewServer）也会拒，这里先拦一道只是为了给出配置层面的措辞。
	if len(config.Users) == 0 {
		return nil, errors.New("tide: users is required; add at least one name:password entry " +
			"(an empty user list accepts ANY client that has the public key, which is not a secret)")
	}
	users := make(map[[16]byte]string, len(config.Users))
	for name, password := range config.Users {
		if password == "" {
			return nil, errors.New("tide: user " + name + " has an empty password")
		}
		users[tideproto.UserIDFromPassword(password)] = name
	}

	srv, err := tideproto.NewServer(&tideproto.ServerConfig{
		PrivateKey: priv,
		Users:      users,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS13,
			// 一个只谈 TLS 却不通告任何 ALPN 的服务端本身就是特征。
			NextProtos: []string{"h2", "http/1.1"},
		},
		CoverAddr:  config.Cover,
		AllowBare:  config.AllowBare,
		Congestion: config.Congestion,
	})
	if err != nil {
		return nil, err
	}
	srv.Handler = func(ctx context.Context, st *tideproto.Stream) {
		tunnel.HandleTCPConn(inbound.NewSocket(
			socks5.ParseAddr(st.RemoteAddr().String()), st, C.TIDE, additions...))
	}
	srv.PacketHandler = func(ctx context.Context, ps *tideproto.PacketStream) {
		handlePackets(ps, tunnel, additions...)
	}

	sl := &Listener{config: config, srv: srv}
	for _, addr := range strings.Split(config.Listen, ",") {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		l, err := lc.Listen(context.Background(), "tcp", addr)
		if err != nil {
			sl.Close()
			return nil, err
		}
		sl.listeners = append(sl.listeners, l)
		go func(l net.Listener) { _ = srv.Serve(l) }(l)
	}
	if len(sl.listeners) == 0 {
		sl.Close()
		return nil, errors.New("tide: listen is empty")
	}
	// QUIC 面是**加速通道**，不是门面（spec §12.6）：QUIC 路径不做掩护转发，
	// 抗主动探测完全靠 TCP 那一条。所以它是可选的，且绝不能单独开放。
	for _, addr := range strings.Split(config.QUICListen, ",") {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		go func(a string) { _ = srv.ServeQUIC(a) }(addr)
	}
	return sl, nil
}

func (l *Listener) Close() error {
	l.closed = true
	if l.srv != nil {
		l.srv.Close()
	}
	var retErr error
	for _, ln := range l.listeners {
		if err := ln.Close(); err != nil {
			retErr = err
		}
	}
	return retErr
}

func (l *Listener) Config() string { return l.config.String() }

func (l *Listener) AddrList() (addrList []net.Addr) {
	for _, ln := range l.listeners {
		addrList = append(addrList, ln.Addr())
	}
	return
}

// loadKeyPair 接受 PEM 内容或文件路径两种写法——配置里两种都常见，
// 只支持一种会让人以为是证书坏了。
func loadKeyPair(certPEMOrPath, keyPEMOrPath string) (tls.Certificate, error) {
	if certPEMOrPath == "" || keyPEMOrPath == "" {
		return tls.Certificate{}, errors.New("tide: certificate and private-key-pem are required")
	}
	read := func(s string) ([]byte, error) {
		if strings.Contains(s, "-----BEGIN") {
			return []byte(s), nil
		}
		return os.ReadFile(s)
	}
	certPEM, err := read(certPEMOrPath)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM, err := read(keyPEMOrPath)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

func handlePackets(ps *tideproto.PacketStream, tunnel C.Tunnel, additions ...inbound.Addition) {
	defer ps.Close()
	for {
		d, err := ps.ReadFrom()
		if err != nil {
			return
		}
		pkt := &packet{ps: ps, addr: d.Addr, data: d.Data}
		tunnel.HandleUDPPacket(inbound.NewPacket(socks5.ParseAddr(d.Addr), pkt, C.TIDE, additions...))
	}
}

type packet struct {
	ps   *tideproto.PacketStream
	addr string
	data []byte
}

func (p *packet) Data() []byte { return p.data }

func (p *packet) WriteBack(b []byte, addr net.Addr) (int, error) {
	target := p.addr
	if addr != nil {
		target = addr.String()
	}
	return p.ps.WriteTo(b, target)
}

func (p *packet) Drop() {}

func (p *packet) LocalAddr() net.Addr { return p.ps.LocalAddr() }
