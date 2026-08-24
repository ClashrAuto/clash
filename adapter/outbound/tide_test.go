package outbound

import (
	"testing"
	"time"

	"github.com/ClashrAuto/coast/common/structure"

	"github.com/ClashrAuto/tide"
)

// tide-server 的启动横幅会打出一整段"贴进客户端就能用"的配置。
// 这个用例就是把那一段原样喂给解码器，逐项断言它真的落进了 TideOption。
//
// ★ 为什么必须逐项断言，而不是"能解码就算过"：mihomo 的解码器对结构体里
// **没有的键是静默丢弃**的——只有带 remain 标签的结构才会报未知键，
// 而 TideOption 没有。所以少一个字段时，解码依然成功、依然零报错，
// 用户照抄横幅贴进去的那一行就那样消失了。
//
// h3 正是这么丢的：服务端开了 -h3 时横幅会打 `h3: true` 并注明"这一行必须跟着开，
// 否则 QUIC 通道会静默失效"，而客户端这边压根没有这个字段。
// 于是横幅让用户加的那一行被无声吃掉，症状恰好就是横幅警告的那一个：
// 两边 ALPN 都是 "h3"，QUIC 握手照样成功，之后服务端把 h3 的 HEADERS 帧当
// TIDE 的 HELLO 解，路径悄悄死掉，客户端静默回落 TCP。
func TestBannerConfigFullyDecodes(t *testing.T) {
	dec := structure.NewDecoder(structure.Option{TagName: "proxy", WeaklyTypedInput: true})
	raw := map[string]any{
		"name":             "tide",
		"server":           "example.com",
		"port":             8443,
		"password":         "pw",
		"public-key":       "AAAA",
		"sni":              "tide.local",
		"udp":              true,
		"quic":             true,
		"h3":               true,
		"skip-cert-verify": true,
		// 横幅里作为建议出现的那一项。
		"redundancy": true,
	}
	var opt TideOption
	if err := dec.Decode(raw, &opt); err != nil {
		t.Fatalf("解码横幅配置失败：%v", err)
	}

	for _, tc := range []struct {
		key string
		got bool
	}{
		{"udp", opt.UDP},
		{"quic", opt.QUIC},
		{"h3", opt.H3},
		{"skip-cert-verify", opt.SkipCertVerify},
		{"redundancy", opt.Redundancy},
	} {
		if !tc.got {
			t.Errorf("横幅里的 %q 没有落进 TideOption —— "+
				"解码器对未知键是静默丢弃的，所以用户不会收到任何提示", tc.key)
		}
	}
	if opt.Server != "example.com" || opt.Port != 8443 ||
		opt.Password != "pw" || opt.PublicKey != "AAAA" || opt.SNI != "tide.local" {
		t.Fatalf("标量字段没解对：%+v", opt)
	}
}

// tideRttMs 是 `/proxies` 里 `tide-rtt` 字段的唯一来源。三条规则都要钉住：
//
//   · 只看 active/degraded —— suspect/dead 的 RTT 不代表这个出站还能不能用；
//   · 下限钳 1 —— 手机两线把 0 定义成「测过且超时」、-1 定义成「没测过」
//     （android NodeListing.kt / ios NodeListing.swift），LAN 上 srtt < 1ms 向下
//     取整成 0 会被当成超时**藏掉**，而那正是这个字段要服务的头号场景；
//   · 没有样本就说没有 —— 编一个 0 出去等于把「刚建的会话」显示成「超时」。
func TestTideRttMs(t *testing.T) {
	ms := func(d time.Duration) tide.PathInfo { return tide.PathInfo{State: "active", RTT: d} }

	if _, ok := tideRttMs(nil); ok {
		t.Fatal("没有路径时不该给值")
	}
	if _, ok := tideRttMs([]tide.PathInfo{{State: "active", RTT: 0}}); ok {
		t.Fatal("还没有探测样本（RTT=0）时不该给值")
	}
	if _, ok := tideRttMs([]tide.PathInfo{
		{State: "suspect", RTT: 5 * time.Millisecond},
		{State: "dead", RTT: 2 * time.Millisecond},
	}); ok {
		t.Fatal("suspect/dead 路径的 RTT 不该被采用")
	}

	// 多路径取最小；degraded 也算（新流仍会选它，见 path.usable）。
	got, ok := tideRttMs([]tide.PathInfo{
		ms(40 * time.Millisecond),
		{State: "degraded", RTT: 7 * time.Millisecond},
		{State: "suspect", RTT: 1 * time.Millisecond},
	})
	if !ok || got != 7 {
		t.Fatalf("该取可用路径里最小的 7ms，得到 %d (ok=%v)", got, ok)
	}

	// LAN 上的亚毫秒 RTT 钳到 1，不能变成「超时」。
	if got, ok = tideRttMs([]tide.PathInfo{ms(300 * time.Microsecond)}); !ok || got != 1 {
		t.Fatalf("亚毫秒 RTT 该钳到 1ms，得到 %d (ok=%v)", got, ok)
	}

	// 上界收在 uint16 里。
	if got, ok = tideRttMs([]tide.PathInfo{ms(90 * time.Second)}); !ok || got != 65535 {
		t.Fatalf("超大 RTT 该封顶 65535，得到 %d (ok=%v)", got, ok)
	}
}
