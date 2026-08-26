package outbound

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/ClashrAuto/coast/common/structure"
	C "github.com/ClashrAuto/coast/constant"

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

// ── 连败熔断（tide_breaker.go）────────────────────────────────────────────────
//
// ★ 两个方向的错法都值得单独钉：不该开时开了 = 好节点被误杀（fallback 组永远切走）；
//   该开不开 = 「一夜 30% 电」原样复发，而且两种都零报错。

func testBreaker(at *time.Time) *dialBreaker {
	b := newDialBreaker("test")
	b.now = func() time.Time { return *at }
	return b
}

func dialFail(t *testing.T, b *dialBreaker) {
	t.Helper()
	if err := b.admit(); err != nil {
		t.Fatalf("这一步应放行，被拒：%v", err)
	}
	b.record(context.DeadlineExceeded)
}

func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	at := time.Unix(1000, 0)
	b := testBreaker(&at)

	dialFail(t, b)
	dialFail(t, b)
	if err := b.admit(); err != nil {
		t.Fatalf("两败之后仍应放行：%v", err)
	}
	b.record(context.DeadlineExceeded) // 第 3 败 → 开

	err := b.admit()
	if err == nil || !strings.Contains(err.Error(), "circuit open") {
		t.Fatalf("三连败后应熔断，得到：%v", err)
	}
	// 冷却剩余要报给调用方（模型/用户拿它决定等多久）。
	if !strings.Contains(err.Error(), "retry in") {
		t.Fatalf("熔断错误里要带剩余秒数：%v", err)
	}
}

func TestBreakerSuccessResetsEverything(t *testing.T) {
	at := time.Unix(1000, 0)
	b := testBreaker(&at)

	dialFail(t, b)
	dialFail(t, b)
	if err := b.admit(); err != nil {
		t.Fatal(err)
	}
	b.record(nil) // 成功 → 清零

	// 又两败：不该开（计数已被成功清掉）。
	dialFail(t, b)
	dialFail(t, b)
	if err := b.admit(); err != nil {
		t.Fatalf("成功复位后两败不该熔断：%v", err)
	}
	b.record(nil)
}

func TestBreakerCanceledNotCounted(t *testing.T) {
	at := time.Unix(1000, 0)
	b := testBreaker(&at)

	for i := 0; i < 10; i++ {
		if err := b.admit(); err != nil {
			t.Fatalf("第 %d 次 canceled 后被拒：%v", i, err)
		}
		b.record(context.Canceled)
	}
	if err := b.admit(); err != nil {
		t.Fatalf("canceled 不是对端的账，不该熔断：%v", err)
	}
	b.record(nil)
}

func TestBreakerHalfOpenSingleProbe(t *testing.T) {
	at := time.Unix(1000, 0)
	b := testBreaker(&at)
	for i := 0; i < 3; i++ {
		dialFail(t, b)
	}

	// 冷却期内全拒。
	at = at.Add(tideBreakerCooldownMin - time.Second)
	if err := b.admit(); err == nil {
		t.Fatal("冷却未到期就放行了")
	}

	// 到期：只放一个探针。
	at = at.Add(2 * time.Second)
	if err := b.admit(); err != nil {
		t.Fatalf("半开态第一个探针应放行：%v", err)
	}
	if err := b.admit(); err == nil || !strings.Contains(err.Error(), "probe") {
		t.Fatalf("探针在途时其余拨号应快速失败，得到：%v", err)
	}

	// 探针成功 → 关，全放行。
	b.record(nil)
	if err := b.admit(); err != nil {
		t.Fatalf("探针成功后应恢复放行：%v", err)
	}
	b.record(nil)
}

func TestBreakerProbeFailureDoublesCooldownWithCap(t *testing.T) {
	at := time.Unix(1000, 0)
	b := testBreaker(&at)
	for i := 0; i < 3; i++ {
		dialFail(t, b)
	}

	cool := tideBreakerCooldownMin
	// 连续探针失败：60s → 120s → 240s → 300s（封顶）→ 300s。
	for round, want := range []time.Duration{2 * tideBreakerCooldownMin,
		4 * tideBreakerCooldownMin, tideBreakerCooldownMax, tideBreakerCooldownMax} {
		at = at.Add(cool + time.Millisecond)
		if err := b.admit(); err != nil {
			t.Fatalf("第 %d 轮到期后探针应放行：%v", round, err)
		}
		b.record(context.DeadlineExceeded)
		if b.cooldown != want {
			t.Fatalf("第 %d 轮重开后冷却应为 %v，得到 %v", round, want, b.cooldown)
		}
		// 重开期间照旧拒。
		if err := b.admit(); err == nil {
			t.Fatalf("第 %d 轮重开后冷却期内不该放行", round)
		}
		cool = want
	}
}

func TestBreakerProbeCanceledReleasesSlot(t *testing.T) {
	at := time.Unix(1000, 0)
	b := testBreaker(&at)
	for i := 0; i < 3; i++ {
		dialFail(t, b)
	}
	at = at.Add(tideBreakerCooldownMin + time.Second)

	if err := b.admit(); err != nil {
		t.Fatal(err)
	}
	b.record(context.Canceled) // 探针被调用方取消：名额必须还回来

	if err := b.admit(); err != nil {
		t.Fatalf("探针取消后名额该释放，下一个拨号应能再探：%v", err)
	}
	b.record(nil)
}

func TestBreakerInflightFailureWhileOpenDoesNotExtend(t *testing.T) {
	at := time.Unix(1000, 0)
	b := testBreaker(&at)
	dialFail(t, b)
	dialFail(t, b)
	// 两个并发拨号都已放行（开之前在途）。
	if err := b.admit(); err != nil {
		t.Fatal(err)
	}
	if err := b.admit(); err != nil {
		t.Fatal(err)
	}
	b.record(context.DeadlineExceeded) // 第 3 败 → 开
	opened := b.openUntil
	b.record(context.DeadlineExceeded) // 开之后才回来的在途失败
	if !b.openUntil.Equal(opened) {
		t.Fatal("开着时收到在途失败不该延长冷却")
	}
}

// 端到端接线：真的构造 Tide 出站指向一个不可达地址，验证熔断在 DialContext 上生效。
//
// ★ 单测拿假时钟验的是状态机；这一条验的是**接线**——admit/record 真的包住了拨号，
//   熔断后的失败是**立即**的（那正是省电的全部来源）。
func TestBreakerEndToEndFastFail(t *testing.T) {
	priv, err := tide.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	tt, err := NewTide(TideOption{
		Name:      "e2e",
		Server:    "192.0.2.1", // TEST-NET-1：保证不可达
		Port:      9,
		Password:  "pw",
		PublicKey: priv.Public().String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tt.Close()

	meta := &C.Metadata{Host: "example.invalid", DstPort: 443}
	for i := 0; i < tideBreakerThreshold; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		if _, err := tt.DialContext(ctx, meta); err == nil {
			t.Fatalf("第 %d 次拨号居然成功了（TEST-NET 不该可达）", i)
		}
		cancel()
	}

	start := time.Now()
	_, err = tt.DialContext(context.Background(), meta)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "circuit open") {
		t.Fatalf("连败后应熔断，得到：%v", err)
	}
	// 快速失败必须是毫秒级——给 CI 抖动留余量，收在 100ms。
	if elapsed > 100*time.Millisecond {
		t.Fatalf("熔断后的失败应是立即的，实测 %v", elapsed)
	}

	// UDP 入口走同一个熔断器。目标给已解析的 IP：ResolveUDP 在 admit 之前
	//（本地解析不该占探针名额），带 Host 的话它会先去查 DNS——测试环境没有解析器，
	// 报出来的是解析错误而不是熔断，断言就验错了对象。
	if _, err := tt.ListenPacketContext(context.Background(),
		&C.Metadata{DstIP: netip.MustParseAddr("192.0.2.99"), DstPort: 443,
			NetWork: C.UDP}); err == nil ||
		!strings.Contains(err.Error(), "circuit open") {
		t.Fatalf("UDP 入口也该被熔断，得到：%v", err)
	}
}
