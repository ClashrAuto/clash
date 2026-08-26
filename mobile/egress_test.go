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
