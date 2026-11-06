//go:build darwin && cgo

package main

import (
	"testing"

	"github.com/ClashrAuto/coast/component/dialer"
)

// setDefaultEgressInterface 是「平台钉口」链路在 Go 侧的落点（Swift 的
// NWPathMonitor → CoastSetDefaultInterface → 这里）。它错了的形态零报错：
// 空名把值清掉 = 决定权交还给 auto-detect（走蜂窝那条错路悄悄回来）；
// 值没存上 = 推送整个是 no-op，日志里连那行 warning 都不会有。
func TestSetDefaultEgressInterface(t *testing.T) {
	orig := dialer.DefaultInterface.Load()
	defer dialer.DefaultInterface.Store(orig) // 别把全局量弄脏给别的测试

	setDefaultEgressInterface("en0")
	if got := dialer.DefaultInterface.Load(); got != "en0" {
		t.Fatalf("推 en0 后 DefaultInterface = %q", got)
	}

	// 空名必须被拒 —— 保持上一个值，不许清空。
	setDefaultEgressInterface("")
	if got := dialer.DefaultInterface.Load(); got != "en0" {
		t.Fatalf("空名把值改掉了：%q", got)
	}

	// 换口要真的换。
	setDefaultEgressInterface("pdp_ip0")
	if got := dialer.DefaultInterface.Load(); got != "pdp_ip0" {
		t.Fatalf("换口后 DefaultInterface = %q", got)
	}

	// 同值重复调（去重早退路径）不能把值弄坏。
	setDefaultEgressInterface("pdp_ip0")
	if got := dialer.DefaultInterface.Load(); got != "pdp_ip0" {
		t.Fatalf("同值重复调之后 DefaultInterface = %q", got)
	}
}

// 换口时清空闲长连接：真换才清，同值与空名都不清 —— 每清一次都是一轮重新握手。
// 这一步缺了的形态是「从蜂窝回到 Wi-Fi 之后，蜂窝上的 tide 路径照样探测一整夜」（2026-11-06
// 真机：基带能耗 10 倍），而日志、界面、流量走向全都正常。
func TestEgressChangeResetsIdleTransportsOnlyOnRealChange(t *testing.T) {
	orig := dialer.DefaultInterface.Load()
	origReset := resetIdleTransports
	defer func() {
		dialer.DefaultInterface.Store(orig)
		resetIdleTransports = origReset
	}()
	calls := 0
	resetIdleTransports = func() int { calls++; return 0 }

	dialer.DefaultInterface.Store("pdp_ip0")
	setDefaultEgressInterface("en0")
	if calls != 1 {
		t.Fatalf("从蜂窝换到 Wi-Fi 应清一次空闲长连接，得到 %d 次", calls)
	}
	setDefaultEgressInterface("en0")
	if calls != 1 {
		t.Fatalf("同值重复推不该再清，得到 %d 次", calls)
	}
	setDefaultEgressInterface("")
	if calls != 1 {
		t.Fatalf("空名不该清，得到 %d 次", calls)
	}
	// 配置里没写 interface-name 时第一次推（旧值为空）也要清：这之前的拨号走的是 auto-detect。
	dialer.DefaultInterface.Store("")
	setDefaultEgressInterface("en0")
	if calls != 2 {
		t.Fatalf("第一次钉口之前的连接也得清，得到 %d 次", calls)
	}
}
