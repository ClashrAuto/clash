package outbound

import (
	"context"
	"crypto/tls"
	"errors"
	"math"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	C "github.com/ClashrAuto/coast/constant"
	"github.com/ClashrAuto/coast/log"

	"github.com/ClashrAuto/tide"
)

// TIDE 出站。整个文件是一层**薄适配**：协议实现全在独立 module github.com/ClashrAuto/tide 里。
//
// 这样安排的原因见 tide/README.md：clash/ 是 mihomo 的 fork，上游变更只能靠 merge 拿。
// 把握手、帧、多路径调度写进 adapter/ 与 transport/ 会让每次合上游都要在大片自有代码上
// 处理冲突；留成三个新文件，合并面就只剩这三个。

type Tide struct {
	*Base
	client *tide.Client
	option *TideOption
	// 连败熔断（见 tide_breaker.go 文件头）。配置重载随 adapter 重建而清零。
	breaker *dialBreaker
	// 顶替登记的键（见 supersedeTideClient），Close 时用来摘掉自己的登记。
	supersedeKey string
	// 上一次打过的路径构成，用来做「只在变化时才打日志」（见 logPathsIfChanged）。
	pathMu    sync.Mutex
	lastPaths string
}

// ── 顶替登记 ────────────────────────────────────────────────────────────
//
// ★★★ mihomo 配置重载不杀存量连接：被换下的 Tide 适配器靠 GC finalizer 关
//   （parser.go 的 NewAutoCloseProxyAdapter），而推送长连接（mtalk/apsd）攥着
//   旧会话的流几天不放——finalizer 永不触发。每次重载漏一个会话，每个会话
//   带着冗余补路 + 探测循环永动。2026-08-29 真机（Android 日用机 ↔ macOS
//   配对电脑）：21 条 ESTABLISHED、66 KB/s 纯协议流量、核心 1300 唤醒/秒。
//   所以在「同一个服务端+用户又建了新客户端」那一刻，把旧客户端的会话切进
//   排水模式：存量流自然走完就关，硬期限兜底（语义见 tide.Session.Drain）。
// ★ 键是 server+userID 而不是节点名：改名序列化出来的还是同一台电脑，
//   而两台不同电脑绝不能互相排水。
// ⚠️ 表里存 *tide.Client 而不是 *Tide：适配器靠 finalizer 回收，注册表攥着
//   适配器指针会让它永远回收不掉——那正是这段要治的病的形状。
var (
	tideSupersedeMu  sync.Mutex
	tideSupersedeMap = map[string]*tide.Client{}
)

// tideDrainHard 是排水的硬期限：正在跑的大传输值得等，但不能永远等——
// 到点强关，推送流断了 app 会在秒级自己重连到新会话。
const tideDrainHard = 30 * time.Minute

func supersedeTideClient(key string, cl *tide.Client) {
	tideSupersedeMu.Lock()
	old := tideSupersedeMap[key]
	tideSupersedeMap[key] = cl
	tideSupersedeMu.Unlock()
	if old != nil && old != cl {
		log.Infoln("[TIDE] superseded old client for %s, draining its session", key)
		old.Drain(tideDrainHard)
	}
}

type TideOption struct {
	BasicOption
	Name     string `proxy:"name"`
	Server   string `proxy:"server"`
	Port     int    `proxy:"port"`
	Password string `proxy:"password"`
	// PublicKey 是服务端静态公钥（base64，X25519 32B + ML-KEM-768 1184B）。
	// 一千六百多个字符确实长，那是后量子的真实价格——客户端必须在第一个包里就完成
	// 封装才能 0-RTT，没有"先问服务端要公钥"的余地。
	PublicKey      string `proxy:"public-key"`
	SNI            string `proxy:"sni,omitempty"`
	SkipCertVerify bool   `proxy:"skip-cert-verify,omitempty"`
	UDP            bool   `proxy:"udp,omitempty"`

	// Bare 请求裸帧模式（内层不加密，安全性完全由外层 TLS 承担）。
	// 服务端只在信道绑定校验通过时才会同意。
	Bare bool `proxy:"bare,omitempty"`
	// QUIC 允许后台挂一条 QUIC 路径。丢包链路上收益很大（实测 p90 从 197ms 到 9ms），
	// UDP 被封时静默回落 TCP，不需要用户操心。
	QUIC     bool `proxy:"quic,omitempty"`
	QUICPort int  `proxy:"quic-port,omitempty"`
	// H3 让 QUIC 面跑在 HTTP/3 之上（tide spec §12.6）。
	//
	// ★ **两端必须一致，而且不一致时完全没有症状。** 两边 ALPN 都是 "h3"，
	// QUIC 握手照样成功，之后服务端把 h3 的 HEADERS 帧当 TIDE 的 HELLO 解，
	// 路径悄悄死掉，客户端按 §8 静默回落 TCP——用户只会觉得"加速通道好像没生效"，
	// 日志里一个字都没有。
	//
	// ⚠️ 这一项从前**根本不存在**：tide-server 的启动横幅在开了 -h3 时会打出
	// `h3: true` 并注明"这一行必须跟着开"，用户照抄贴进配置，而 mihomo 的解码器
	// 对结构体里没有的键是**静默丢弃**的（只有带 remain 标签的结构才会报未知键）。
	// 于是横幅让用户加的那一行被无声吃掉，症状恰好就是横幅警告的那一个。
	H3 bool `proxy:"h3,omitempty"`
	// Redundancy 常驻两条路径：路径死掉时不用重连，流直接切过去。
	// 移动网络建议开，稳定有线网没必要。
	Redundancy bool `proxy:"redundancy,omitempty"`
	// HaltOnFail：连败熔断进入 halt 档（见 tide_breaker.go）——熔断后冷却 1 小时
	// 而不是 5 分钟封顶，并投一条事件让平台侧发系统通知。
	// 「我的电脑」卡片上把备用节点选成「不使用」时由 ConfigBuilder 下发。
	HaltOnFail bool `proxy:"halt-on-fail,omitempty"`
	// SessionGrace 是所有路径都断了之后会话还能活多久（秒）。这段时间里
	// 服务端替你保留着上游连接，所以 Wi-Fi 切换、基站切换都不会断连接。
	SessionGrace int `proxy:"session-grace,omitempty"`
	// Congestion 指定 TCP 路径的拥塞控制（Linux 专有）。留空 = 不动系统默认。
	// ⚠️ 别想当然填 "bbr"：实测双向 5% 丢包下它把 p99 从 125ms 抬到 620ms——
	//    内核里的 bbr 是 v1，对随机丢包的处理正是它的弱点。
	Congestion string `proxy:"congestion,omitempty"`
}

func (t *Tide) DialContext(ctx context.Context, metadata *C.Metadata) (C.Conn, error) {
	// ★ 连败熔断（见 tide_breaker.go 文件头）：对端长时间不在时快速失败，
	//   别让每条新流都付一遍完整的拨号超时——那正是「一夜 30% 电」的形态。
	if err := t.breaker.admit(); err != nil {
		return nil, err
	}
	c, err := t.client.DialContext(ctx, "tcp", metadata.RemoteAddress())
	t.breaker.record(err)
	if err != nil {
		return nil, err
	}
	t.logPathsIfChanged()
	return NewConn(c, t), nil
}

func (t *Tide) ListenPacketContext(ctx context.Context, metadata *C.Metadata) (C.PacketConn, error) {
	// ★ ResolveUDP 在 admit 之前：它是本地解析，失败不该记到对端头上，
	//   也不该占掉半开态唯一的探针名额。admit 必须紧贴真正的网络拨号。
	if err := t.ResolveUDP(ctx, metadata); err != nil {
		return nil, err
	}
	if err := t.breaker.admit(); err != nil {
		return nil, err
	}
	ps, err := t.client.DialPacket(ctx, metadata.RemoteAddress())
	t.breaker.record(err)
	if err != nil {
		return nil, err
	}
	t.logPathsIfChanged()
	return NewPacketConn(&tidePacketConn{ps: ps}, t), nil
}

// ProxyInfo implements C.ProxyAdapter
func (t *Tide) ProxyInfo() C.ProxyInfo {
	info := t.Base.ProxyInfo()
	info.DialerProxy = t.option.DialerProxy
	return info
}

// logPathsIfChanged 把当前的路径构成（每条路径的 kind，如 tcp/tcp、tcp/quic）
// 打到 debug 日志，且**只在构成变化时打一次**，不随连接数刷屏。
//
// ★ 加它的理由是一次差点交付出去的假缺陷报告。tide 自己是知道路径状态的
//   （Session.Paths() 带 Kind），但**没有任何地方把它露出来**，于是「QUIC 到底
//   起没起」只能靠抓包，而抓包这条路我 2026-08-08 连着走错三次：
//     · `ss -unp | grep <服务端>:8443` —— 恒为 0。QUIC 用的是**未连接**的 UDP
//       套接字（ListenUDP + WriteToUDP），内核里压根没有"对端地址"这一项，
//       按对端去 grep 永远匹配不到。
//     · `ss -unp | grep pid=<核心>` —— 同样是 0，未连接套接字默认不显示。
//     · 于是我依次怀疑到"没设 quic-port"（QUICPort=0 本就复用 Server 端口）、
//       "服务端没开 QUIC 面"（TIDE_QUIC_LISTEN=:8443 设了），两次都被证伪。
//   最后是 tcpdump 看见 1409 字节的 QUIC 包正双向跑着 —— 多路径一直好好的，
//   坏的是我的观测手段。有了这行日志，`log-level: debug` 就能直接回答这个问题。
func (t *Tide) logPathsIfChanged() {
	// ★ 非 debug 直接返回。这个函数挂在**每一次 DialContext / ListenPacketContext**
	//   上，而下面每跑一遍都要：取会话锁做路径快照、分配切片、排序、拼字符串。
	//   繁忙代理每秒几百条连接时这是实打实的分配churn，而它的产出（一行 debug 日志）
	//   在非 debug 级别下根本不会被打印。
	//   顺带说明为什么不能只靠 log.Debugln 自己拦：它是**无条件**把事件塞进 logCh
	//   再由 print 判级别的，级别不够时 newLog 的分配和 channel 发送照样发生。
	if log.Level() > log.DEBUG {
		return
	}
	s := t.client.CurrentSession()
	if s == nil {
		return
	}
	kinds := make([]string, 0, 4)
	for _, p := range s.Paths() {
		kinds = append(kinds, p.Kind)
	}
	if len(kinds) == 0 {
		return
	}
	sort.Strings(kinds) // 顺序无意义，排序后才能拿来比"变没变"
	sum := strings.Join(kinds, "+")
	t.pathMu.Lock()
	changed := sum != t.lastPaths
	if changed {
		t.lastPaths = sum
	}
	t.pathMu.Unlock()
	if changed {
		log.Debugln("[TIDE] %s 路径构成: %s（共 %d 条，累计建立 %d 次）",
			t.Name(), sum, len(kinds), s.PathsEstablished())
	}
}

// tideRttMs 从路径快照里挑出**新流会走的那批路径**（active/degraded，与 path.usable
// 同一判据）中最小的平滑 RTT，毫秒。ok=false 表示没有任何带样本的可用路径。
//
// ★ 下限钳到 1：手机两线把 delay==0 定义成「测过且超时」、-1 定义成「没测过」
//   （android NodeListing.kt / ios NodeListing.swift 的契约）。环回或千兆 LAN 上
//   srtt 完全可能不足 1ms，向下取整成 0 会被那边当成超时藏掉。
func tideRttMs(paths []tide.PathInfo) (uint16, bool) {
	best := time.Duration(0)
	for _, p := range paths {
		if p.State != "active" && p.State != "degraded" {
			continue // suspect/dead：新流不会选它，它的 RTT 不代表这个出站
		}
		if p.RTT <= 0 {
			continue // 探测还没拿到第一个样本
		}
		if best == 0 || p.RTT < best {
			best = p.RTT
		}
	}
	if best == 0 {
		return 0, false
	}
	ms := int64(best / time.Millisecond)
	if ms < 1 {
		ms = 1
	}
	if ms > math.MaxUint16 {
		ms = math.MaxUint16
	}
	return uint16(ms), true
}

// PathRTTMs 返回当前会话到 TIDE 服务端的路径 RTT（毫秒），供 REST 层以 `tide-rtt`
// 字段暴露（adapter.Proxy.MarshalJSON）。
//
// ★ 它和 URLTest 的 history 延迟回答的是**两个不同的问题**：history 是「经这个出站
//   访问测速 URL 的全程」——对「我的电脑」这类 TIDE 出站，请求进了对端还要过对端
//   自己的分流规则，测速 URL（gstatic）命中 PROXY 时量到的是对端出口节点的延迟
//   （局域网里的电脑显示 400+ms 就是这么来的）；而这里是协议自己的 PATH_PROBE
//   量出来的**本机↔服务端这一跳**。客户端对 TIDE 节点展示后者，健康检查仍用前者
//   （出口坏了要能判死，见手机侧 nodeDelay 的注释）。
// ★ 没有会话时返回 false 而不是去建一条：这是只读的观测口，挂在每次 /proxies
//   序列化上，让它拨号等于让轮询本身产生流量。
func (t *Tide) PathRTTMs() (uint16, bool) {
	s := t.client.CurrentSession()
	if s == nil {
		return 0, false
	}
	return tideRttMs(s.Paths())
}

// Close implements C.ProxyAdapter
func (t *Tide) Close() error {
	// 只摘自己的登记：finalizer 迟到时表里早已是新客户端，不能误删它。
	tideSupersedeMu.Lock()
	if t.supersedeKey != "" && tideSupersedeMap[t.supersedeKey] == t.client {
		delete(tideSupersedeMap, t.supersedeKey)
	}
	tideSupersedeMu.Unlock()
	return t.client.Close()
}

func NewTide(option TideOption) (*Tide, error) {
	addr := net.JoinHostPort(option.Server, strconv.Itoa(option.Port))
	if option.PublicKey == "" {
		return nil, errors.New("tide: public-key is required")
	}
	pub, err := tide.ParsePublicKey(option.PublicKey)
	if err != nil {
		return nil, err
	}

	out := &Tide{
		Base: NewBase(BaseOption{
			Name:         option.Name,
			Addr:         addr,
			Type:         C.Tide,
			ProviderName: option.ProviderName,
			UDP:          option.UDP,
			TFO:          option.TFO,
			MPTCP:        option.MPTCP,
			Interface:    option.Interface,
			RoutingMark:  option.RoutingMark,
			Prefer:       option.IPVersion,
		}),
		option: &option,
	}
	out.breaker = newDialBreaker(option.Name, option.HaltOnFail)
	out.dialer = option.NewDialer(out.DialOptions())

	sni := option.SNI
	if sni == "" {
		sni = option.Server
	}
	cfg := &tide.ClientConfig{
		Server:     addr,
		PublicKey:  pub,
		ServerName: sni,
		TLSConfig: &tls.Config{
			ServerName:         sni,
			InsecureSkipVerify: option.SkipCertVerify,
			MinVersion:         tls.VersionTLS13,
		},
		Bare:       option.Bare,
		EnableQUIC: option.QUIC,
		H3:         option.H3,
		QUICPort:   option.QUICPort,
		Redundancy: option.Redundancy,
		// 走 clash 的 dialer，接口绑定 / fwmark / DNS 策略才会生效。
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return out.dialer.DialContext(ctx, network, address)
		},
		// ★ QUIC/h3 那条路的 UDP 套接字也必须从 clash 的 dialer 里出来。
		//
		// 少了这一条，tide 会用 quic.DialAddr 自己开一个裸 UDP 套接字，于是绕过了
		// 接口绑定、fwmark，以及最要命的"这是内核自身流量"的标记。开了 TUN 的机器上
		// 后果是环路：客户端发往服务端的 QUIC 包被**自己的 TUN** 捕获绕回路由器，
		// 嗅探器再从 QUIC ClientHello 里读出 SNI 把目的地改写成那个名字（比如 tide.local），
		// 然后解析失败，日志刷屏：
		//   [UDP] dial ... --> tide.local:8443 error: can't resolve ip: couldn't find ip
		// TCP 路径完全正常，所以现象看着像"域名配错了"，其实跟配置无关。
		// 2026-08-07 用户在 Coast 1.0.974 + 增强(TUN) 上实测到的就是这个。
		ListenPacket: func(ctx context.Context, address string) (net.PacketConn, error) {
			ua, err := net.ResolveUDPAddr("udp", address)
			if err != nil {
				return nil, err
			}
			ap, ok := netip.AddrFromSlice(ua.IP)
			if !ok {
				return nil, errors.New("tide: 无法解析 QUIC 目的地址 " + address)
			}
			return out.dialer.ListenPacket(ctx, "udp", "",
				netip.AddrPortFrom(ap.Unmap(), uint16(ua.Port)))
		},
	}
	if option.SessionGrace > 0 {
		cfg.SessionGrace = time.Duration(option.SessionGrace) * time.Second
	}
	cfg.Congestion = option.Congestion
	cfg.UserID = tide.UserIDFromPassword(option.Password)

	cl, err := tide.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	out.client = cl
	// 顶替上一个同服务端+同用户的客户端（配置重载的每一轮都会走到这里）。
	out.supersedeKey = addr + "|" + string(cfg.UserID[:])
	supersedeTideClient(out.supersedeKey, cl)
	return out, nil
}

// tidePacketConn 把 TIDE 的 UDP 关联包装成 net.PacketConn。
//
// TIDE 的数据报走在已认证的会话内，归属是结构决定的——不像 SOCKS5 UDP 中继那样
// 要靠客户端申报来源地址来做 addr→user 归属（那条链路上申报错了不会报错，
// 只是规则对 UDP 静默失配）。
type tidePacketConn struct {
	ps *tide.PacketStream
}

func (c *tidePacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	d, err := c.ps.ReadFrom()
	if err != nil {
		return 0, nil, err
	}
	n := copy(p, d.Data)
	addr, err := net.ResolveUDPAddr("udp", d.Addr)
	if err != nil {
		// 对端给了个域名形式的来源地址（正常情况下不会）。返回一个占位地址
		// 好过丢掉这个数据报。
		return n, &net.UDPAddr{IP: net.IPv4zero}, nil
	}
	return n, addr, nil
}

func (c *tidePacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	return c.ps.WriteTo(p, addr.String())
}

func (c *tidePacketConn) Close() error                       { return c.ps.Close() }
func (c *tidePacketConn) LocalAddr() net.Addr                { return c.ps.LocalAddr() }
func (c *tidePacketConn) SetDeadline(t time.Time) error      { return c.ps.SetReadDeadline(t) }
func (c *tidePacketConn) SetReadDeadline(t time.Time) error  { return c.ps.SetReadDeadline(t) }
func (c *tidePacketConn) SetWriteDeadline(t time.Time) error { return nil }
