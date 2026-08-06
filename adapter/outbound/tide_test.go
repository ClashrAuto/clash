package outbound

import (
	"testing"

	"github.com/ClashrAuto/coast/common/structure"
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
