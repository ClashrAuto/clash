package inbound

import (
	"testing"

	"github.com/ClashrAuto/coast/common/structure"
)

// 入站选项也要逐项断言真的落进了结构体。
//
// ★ 理由和出站那边一样：mihomo 的解码器对结构体里**没有的键是静默丢弃**的
// （只有带 remain 标签的结构才会报未知键），所以少一个字段时解码依然成功、
// 依然零报错，用户写进配置的那一行就那样消失了。
//
// h3 正是这么缺的：tide 库里 Server.ServeH3 早就存在、有测试、也被 spec §12.6
// 记成"QUIC 面抗主动探测"的解法，但整条入站配置链（inbound 选项 → listener 配置
// → server.go）从头到尾没有这个字段，于是 Coast 自建的 TIDE 服务端**根本没办法
// 打开 h3**——它只能跑 ServeQUIC，而那条路对非 TIDE 的 QUIC 客户端只能沉默。
// 一台在 TCP/443 上服务 HTTPS 的主机，在 UDP/443 上跑一个"握手能成、却不回任何
// h3 请求"的端点，正是探测方要找的那种异常。
func TestTideInboundOptionsFullyDecode(t *testing.T) {
	dec := structure.NewDecoder(structure.Option{TagName: "inbound", WeaklyTypedInput: true})
	raw := map[string]any{
		"name":            "tide-in",
		"listen":          "0.0.0.0",
		"port":            8443,
		"users":           map[string]any{"alice": "pw"},
		"private-key":     "seed",
		"certificate":     "cert",
		"private-key-pem": "key",
		"cover":           "127.0.0.1:8080",
		"quic-listen":     "0.0.0.0:8443",
		"h3":              true,
		"allow-bare":      true,
		"congestion":      "cubic",
	}
	var opt TideOption
	if err := dec.Decode(raw, &opt); err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	if !opt.H3 {
		t.Error("h3 没有落进 TideOption —— 解码器对未知键静默丢弃，用户不会收到任何提示；" +
			"结果是自建服务端只能跑沉默的 ServeQUIC，拿不到 §12.6 的主动探测防御")
	}
	if !opt.AllowBare {
		t.Error("allow-bare 没有落进 TideOption")
	}
	if opt.QUICListen != "0.0.0.0:8443" || opt.Cover != "127.0.0.1:8080" ||
		opt.PrivateKey != "seed" || opt.Congestion != "cubic" {
		t.Fatalf("标量字段没解对：%+v", opt)
	}
	if opt.Users["alice"] != "pw" {
		t.Fatalf("users 没解对：%+v", opt.Users)
	}

	// h3 必须一路传到 listener 的配置结构里——中间断一节，症状同样是静默的。
	in, err := NewTide(&opt)
	if err != nil {
		t.Fatalf("NewTide 失败：%v", err)
	}
	if !in.vs.H3 {
		t.Fatal("h3 停在 TideOption 上，没有传进 LC.TideServer")
	}
}
