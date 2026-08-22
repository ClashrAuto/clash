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
	"github.com/ClashrAuto/coast/log"
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
	// ★★ 把认证到的用户名挂到每一条连接上。
	//
	//   握手时服务端已经按 users 表比对出了是谁（`Session.user`），但在此之前那个结论
	//   **只用于放行、不往上层传** —— 于是一台服务端上所有客户端的连接在
	//   `/connections` 里长得完全一样，上层想回答「哪台设备在线、它跑了多少流量」
	//   就只能靠猜。桌面端要按设备列表管理远程客户端，靠的正是这个字段。
	//
	// ⚠️ 每条连接**现取现拼**，不能在外面拼一次共用：`additions` 是所有连接共享的切片，
	//   直接 append 会写进同一块底层数组（socks/udp.go 那边为同一件事记过一次）。
	withUser := func(uid [16]byte) []inbound.Addition {
		name, ok := users[uid]
		if !ok {
			return additions // 认不出就别编一个名字出来，宁可没有
		}
		out := make([]inbound.Addition, 0, len(additions)+1)
		out = append(out, additions...)
		return append(out, inbound.WithInUser(name))
	}
	srv.Handler = func(ctx context.Context, st *tideproto.Stream) {
		tunnel.HandleTCPConn(inbound.NewSocket(
			socks5.ParseAddr(st.RemoteAddr().String()), st, C.TIDE, withUser(st.User())...))
	}
	srv.PacketHandler = func(ctx context.Context, ps *tideproto.PacketStream) {
		handlePackets(ps, tunnel, withUser(ps.User())...)
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
		// ★ TCP 那条 Serve 的返回值同样不能丢。
		//
		// 第 36 轮把 QUIC 那条的错误捞出来打了日志，却漏了这一条——而它更严重：
		// QUIC 挂了只是没有加速通道，TCP 这条 Serve 一旦返回，**整个入站就永久
		// 停止接受连接**了。进程还在、端口还 listen 着，现象只是"服务好好的、
		// 但再也连不上"。库那边已经把 EMFILE 一类临时错误改成退避重试，
		// 走到这里的就是真正不可恢复的了，必须说出来。
		go func(l net.Listener) {
			if err := srv.Serve(l); err != nil {
				log.Errorln("[TIDE] listener on %s stopped accepting: %s", l.Addr(), err)
			}
		}(l)
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
		// ★ h3 模式与原生 QUIC 的区别不是性能，是**抗主动探测**：
		// ServeQUIC 对非 TIDE 的 QUIC 客户端只能沉默，而 ServeH3 会把非 TIDE 的
		// h3 请求反代到掩护源站。一台在 TCP/443 上服务 HTTPS 的主机，
		// 在 UDP/443 上跑一个"握手能成、却不回任何 h3 请求"的端点是说不通的，
		// 探测方一次普通 h3 请求就能挑出来。
		// ★ 绑定失败**必须**说出来。这里从前是 `_ =`，于是 quic-listen 绑不上
		// （端口被占、权限不足……）时整条链路一声不吭：服务端照常起来，
		// 客户端拨 QUIC 失败后按 tide spec §8 **静默**回落 TCP——那是协议要求的行为，
		// 不会有任何报错。用户只会觉得"加速通道没生效"，而日志里一个字都没有。
		go func(a string, h3 bool) {
			var err error
			if h3 {
				err = srv.ServeH3(a)
			} else {
				err = srv.ServeQUIC(a)
			}
			// 正常停机时 Serve 返回 nil；非 nil 才是真出事了。
			if err != nil {
				log.Errorln("[TIDE] QUIC listen on %s failed: %s "+
					"(the accelerator is now absent; clients fall back to TCP silently)", a, err)
			}
		}(addr, config.H3)
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
