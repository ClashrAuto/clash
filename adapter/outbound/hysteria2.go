package outbound

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	N "github.com/ClashrAuto/coast/common/net"
	"github.com/ClashrAuto/coast/common/utils"
	"github.com/ClashrAuto/coast/component/ca"
	"github.com/ClashrAuto/coast/component/resolver"
	C "github.com/ClashrAuto/coast/constant"
	"github.com/ClashrAuto/coast/log"
	"github.com/ClashrAuto/coast/transport/tuic/common"

	"github.com/metacubex/http"
	"github.com/metacubex/quic-go"
	qtls "github.com/metacubex/sing-quic"
	"github.com/metacubex/sing-quic/hysteria2"
	"github.com/metacubex/sing-quic/hysteria2/realm"
	M "github.com/metacubex/sing/common/metadata"
	"github.com/metacubex/tls"
)

const minHopInterval = 5
const defaultHopInterval = 30

type Hysteria2 struct {
	*Base

	option *Hysteria2Option
	// ★ 指针可换：ResetTransport 要整个换掉客户端（见那里的注释），拨号路径无锁地读当前这个。
	client atomic.Pointer[hysteria2.Client]
	// 建客户端用的参数，换新客户端时原样再建一个。
	clientOptions hysteria2.ClientOptions
	resetMu       sync.Mutex // 串行化 ResetTransport 与 Close
	closed        bool       // Close 之后不再换新客户端（resetMu 下读写）
}

type Hysteria2Option struct {
	BasicOption
	Name              string     `proxy:"name"`
	Server            string     `proxy:"server"`
	Port              int        `proxy:"port,omitempty"`
	Ports             string     `proxy:"ports,omitempty"`
	HopInterval       string     `proxy:"hop-interval,omitempty"`
	Up                string     `proxy:"up,omitempty"`
	Down              string     `proxy:"down,omitempty"`
	Password          string     `proxy:"password,omitempty"`
	Obfs              string     `proxy:"obfs,omitempty"`
	ObfsPassword      string     `proxy:"obfs-password,omitempty"`
	ObfsMinPacketSize int        `proxy:"obfs-min-packet-size,omitempty"`
	ObfsMaxPacketSize int        `proxy:"obfs-max-packet-size,omitempty"`
	SNI               string     `proxy:"sni,omitempty"`
	ECHOpts           ECHOptions `proxy:"ech-opts,omitempty"`
	SkipCertVerify    bool       `proxy:"skip-cert-verify,omitempty"`
	NameCertVerify    string     `proxy:"name-cert-verify,omitempty"`
	Fingerprint       string     `proxy:"fingerprint,omitempty"`
	Certificate       string     `proxy:"certificate,omitempty"`
	PrivateKey        string     `proxy:"private-key,omitempty"`
	ALPN              []string   `proxy:"alpn,omitempty"`
	CWND              int        `proxy:"cwnd,omitempty"`
	BBRProfile        string     `proxy:"bbr-profile,omitempty"`
	UdpMTU            int        `proxy:"udp-mtu,omitempty"`

	RealmOpts Hysteria2RealmOption `proxy:"realm-opts,omitempty"`

	// quic-go special config
	InitialStreamReceiveWindow     uint64 `proxy:"initial-stream-receive-window,omitempty"`
	MaxStreamReceiveWindow         uint64 `proxy:"max-stream-receive-window,omitempty"`
	InitialConnectionReceiveWindow uint64 `proxy:"initial-connection-receive-window,omitempty"`
	MaxConnectionReceiveWindow     uint64 `proxy:"max-connection-receive-window,omitempty"`
}

type Hysteria2RealmOption struct {
	Enable      bool     `proxy:"enable,omitempty"`
	ServerURL   string   `proxy:"server-url,omitempty"`
	Token       string   `proxy:"token,omitempty"`
	RealmID     string   `proxy:"realm-id,omitempty"`
	STUNServers []string `proxy:"stun-servers,omitempty"`

	// for ServerURL
	SNI            string   `proxy:"sni,omitempty"`
	SkipCertVerify bool     `proxy:"skip-cert-verify,omitempty"`
	NameCertVerify string   `proxy:"name-cert-verify,omitempty"`
	Fingerprint    string   `proxy:"fingerprint,omitempty"`
	Certificate    string   `proxy:"certificate,omitempty"`
	PrivateKey     string   `proxy:"private-key,omitempty"`
	ALPN           []string `proxy:"alpn,omitempty"`
}

func (h *Hysteria2) DialContext(ctx context.Context, metadata *C.Metadata) (_ C.Conn, err error) {
	c, err := h.client.Load().DialConn(ctx, M.ParseSocksaddrHostPort(metadata.String(), metadata.DstPort))
	if err != nil {
		return nil, err
	}
	return NewConn(c, h), nil
}

func (h *Hysteria2) ListenPacketContext(ctx context.Context, metadata *C.Metadata) (_ C.PacketConn, err error) {
	if err = h.ResolveUDP(ctx, metadata); err != nil {
		return nil, err
	}
	pc, err := h.client.Load().ListenPacket(ctx)
	if err != nil {
		return nil, err
	}
	if pc == nil {
		return nil, errors.New("packetConn is nil")
	}
	return NewPacketConn(N.NewThreadSafePacketConn(pc), h), nil
}

// ResetTransport 换一个新客户端，旧的连同它的 QUIC 连接一起关掉，下一次拨号重新握手
// （醒来对账用，判据与理由见 tunnel/wake.go）。
//
// 只能整个换：sing-quic 的 CloseWithError 是**终态**——它把 connErr 置上，此后
// offer() 一律返回那个错误，客户端再也拨不出去。它没有「只关当前连接、客户端照用」的
// 公开入口，所以按建它时的参数原样再建一个（NewClient 只建结构、不碰网络）。
//
// QUICConfig 每次给一份拷贝：NewClient 会无条件写其中两个字段，与旧客户端共用同一份
// 就是一次数据竞争（quic-go 拨号时自己还会再克隆，浅拷贝足够）。
func (h *Hysteria2) ResetTransport() {
	h.resetMu.Lock()
	defer h.resetMu.Unlock()
	if h.closed {
		return
	}
	opts := h.clientOptions
	if opts.QUICConfig != nil {
		qc := *opts.QUICConfig
		opts.QUICConfig = &qc
	}
	fresh, err := hysteria2.NewClient(opts)
	if err != nil {
		log.Warnln("[Hysteria2] %s: transport reset failed, keeping the old client: %v", h.Name(), err)
		return
	}
	if old := h.client.Swap(fresh); old != nil {
		_ = old.CloseWithError(errors.New("transport reset after sleep"))
	}
}

// Close implements C.ProxyAdapter
func (h *Hysteria2) Close() error {
	h.resetMu.Lock()
	defer h.resetMu.Unlock()
	h.closed = true
	if c := h.client.Load(); c != nil {
		return c.CloseWithError(errors.New("proxy removed"))
	}
	return nil
}

// ProxyInfo implements C.ProxyAdapter
func (h *Hysteria2) ProxyInfo() C.ProxyInfo {
	info := h.Base.ProxyInfo()
	info.DialerProxy = h.option.DialerProxy
	return info
}

func NewHysteria2(option Hysteria2Option) (*Hysteria2, error) {
	addr := net.JoinHostPort(option.Server, strconv.Itoa(option.Port))
	outbound := &Hysteria2{
		Base: NewBase(BaseOption{
			Name:         option.Name,
			Addr:         addr,
			Type:         C.Hysteria2,
			ProviderName: option.ProviderName,
			UDP:          true,
			Interface:    option.Interface,
			RoutingMark:  option.RoutingMark,
			Prefer:       option.IPVersion,
		}),
		option: &option,
	}
	outbound.dialer = option.NewDialer(outbound.DialOptions())

	var salamanderPassword string
	var geckoPassword string
	var geckoMinPacketSize, geckoMaxPacketSize int
	if len(option.Obfs) > 0 {
		if option.ObfsPassword == "" {
			return nil, errors.New("missing obfs password")
		}
		switch option.Obfs {
		case hysteria2.ObfsTypeSalamander:
			salamanderPassword = option.ObfsPassword
		case hysteria2.ObfsTypeGecko:
			geckoPassword = option.ObfsPassword
			geckoMinPacketSize = option.ObfsMinPacketSize
			geckoMaxPacketSize = option.ObfsMaxPacketSize
		default:
			return nil, fmt.Errorf("unknown obfs type: %s", option.Obfs)
		}
	}

	serverName := option.Server
	if option.SNI != "" {
		serverName = option.SNI
	}

	tlsConfig, err := ca.GetTLSConfig(ca.Option{
		TLSConfig: &tls.Config{
			ServerName:         serverName,
			InsecureSkipVerify: option.SkipCertVerify,
			MinVersion:         tls.VersionTLS13,
		},
		Fingerprint:    option.Fingerprint,
		NameCertVerify: option.NameCertVerify,
		Certificate:    option.Certificate,
		PrivateKey:     option.PrivateKey,
	})
	if err != nil {
		return nil, err
	}

	if option.ALPN != nil { // structure's Decode will ensure value not nil when input has value even it was set an empty array
		tlsConfig.NextProtos = option.ALPN
	}

	tlsClientConfig := tlsConfig
	echConfig, err := option.ECHOpts.Parse()
	if err != nil {
		return nil, err
	}

	if option.UdpMTU == 0 {
		// "1200" from quic-go's MaxDatagramSize
		// "-3" from quic-go's DatagramFrame.MaxDataLen
		option.UdpMTU = 1200 - 3
	}

	quicConfig := &quic.Config{
		InitialStreamReceiveWindow:     option.InitialStreamReceiveWindow,
		MaxStreamReceiveWindow:         option.MaxStreamReceiveWindow,
		InitialConnectionReceiveWindow: option.InitialConnectionReceiveWindow,
		MaxConnectionReceiveWindow:     option.MaxConnectionReceiveWindow,
	}

	clientOptions := hysteria2.ClientOptions{
		Context:            context.TODO(),
		Logger:             log.SingLogger,
		SendBPS:            utils.StringToBps(option.Up),
		ReceiveBPS:         utils.StringToBps(option.Down),
		SalamanderPassword: salamanderPassword,
		GeckoPassword:      geckoPassword,
		GeckoMinPacketSize: geckoMinPacketSize,
		GeckoMaxPacketSize: geckoMaxPacketSize,
		Password:           option.Password,
		TLSConfig:          tlsClientConfig,
		QUICConfig:         quicConfig,
		UDPDisabled:        false,
		UdpMTU:             option.UdpMTU,
		ServerAddress:      M.ParseSocksaddr(addr),
		PacketListener:     outbound.dialer,
		QuicDialer: qtls.QuicDialerFunc(func(ctx context.Context, addr string, dialer qtls.PacketDialer, tlsCfg *tls.Config, cfg *quic.Config, early bool) (net.PacketConn, *quic.Conn, error) {
			err := echConfig.ClientHandle(ctx, tlsCfg)
			if err != nil {
				return nil, nil, err
			}
			return common.DialQuic(ctx, addr, outbound.DialOptions(), dialer, tlsCfg, cfg, early)
		}),
		SetBBRCongestion: func(quicConn *quic.Conn) {
			common.SetCongestionController(quicConn, "bbr", option.CWND, option.BBRProfile)
		},
	}

	var serverPorts []uint16
	if option.Ports != "" {
		ranges, err := utils.NewUnsignedRanges[uint16](option.Ports)
		if err != nil {
			return nil, err
		}
		ranges.Range(func(port uint16) bool {
			serverPorts = append(serverPorts, port)
			return true
		})
		if len(serverPorts) > 0 {
			hopRange, err := utils.NewUnsignedRange[uint64](option.HopInterval)
			if err != nil {
				return nil, err
			}
			start, end := hopRange.Start(), hopRange.End()
			if start == 0 {
				start = defaultHopInterval
			} else if start < minHopInterval {
				start = minHopInterval
			}
			if end < start {
				end = start
			}
			clientOptions.HopInterval = time.Duration(start) * time.Second
			clientOptions.HopIntervalMax = time.Duration(end) * time.Second
			clientOptions.ServerPorts = serverPorts
		}
	}
	if option.Port == 0 && len(serverPorts) == 0 {
		return nil, errors.New("invalid port")
	}

	if option.RealmOpts.Enable {
		httpTLSClientConfig, err := ca.GetTLSConfig(ca.Option{
			TLSConfig: &tls.Config{
				ServerName:         option.RealmOpts.SNI,
				InsecureSkipVerify: option.RealmOpts.SkipCertVerify,
				NextProtos:         option.RealmOpts.ALPN,
			},
			Fingerprint:    option.RealmOpts.Fingerprint,
			NameCertVerify: option.RealmOpts.NameCertVerify,
			Certificate:    option.RealmOpts.Certificate,
			PrivateKey:     option.RealmOpts.PrivateKey,
		})
		if err != nil {
			return nil, err
		}
		clientOptions.RealmOptions = &realm.Options{
			ServerURL:   option.RealmOpts.ServerURL,
			Token:       option.RealmOpts.Token,
			RealmID:     option.RealmOpts.RealmID,
			STUNServers: option.RealmOpts.STUNServers,
			HTTPClient: &http.Client{
				Transport: &http.Transport{
					DialContext:     outbound.dialer.DialContext,
					TLSClientConfig: httpTLSClientConfig,
					// from http.DefaultTransport
					ForceAttemptHTTP2:     true,
					MaxIdleConns:          100,
					IdleConnTimeout:       90 * time.Second,
					TLSHandshakeTimeout:   10 * time.Second,
					ExpectContinueTimeout: 1 * time.Second,
				},
			},
			Resolver: func(ctx context.Context, host string, ipv4, ipv6 bool) ([]netip.Addr, error) {
				if ipv4 && !ipv6 {
					return resolver.LookupIPv4WithResolver(ctx, host, resolver.ProxyServerHostResolver)
				} else if ipv6 && !ipv4 {
					return resolver.LookupIPv6WithResolver(ctx, host, resolver.ProxyServerHostResolver)
				}
				return resolver.LookupIPWithResolver(ctx, host, resolver.ProxyServerHostResolver)
			},
			Logger: log.SingLogger,
		}
	}

	client, err := hysteria2.NewClient(clientOptions)
	if err != nil {
		return nil, err
	}
	outbound.client.Store(client)
	outbound.clientOptions = clientOptions

	return outbound, nil
}
