package adapter

import (
	"encoding/json"
	"testing"

	"github.com/ClashrAuto/coast/adapter/outbound"
)

// rttAdapter 模拟 TIDE 出站：一个普通出站 + PathRTTMs 观测口。
type rttAdapter struct {
	outbound.ProxyAdapter
	ms  uint16
	has bool
}

func (a rttAdapter) PathRTTMs() (uint16, bool) { return a.ms, a.has }

// ★★ 走 parser 真实的包装路径（NewAutoCloseProxyAdapter → NewProxy → MarshalJSON）。
//
// 2026-09-26 查清：parser 给每个出站都套 autoCloseProxyAdapter，而那层壳按接口嵌入，
// PathRTTMs 穿不过来 —— `/proxies` 里 `tide-rtt` 在真实配置下一次都没出现过。
// 原来只有 outbound 包里测 tideRttMs 的单测，它绕开了这层壳，所以一直是绿的。
func TestTideRttSurvivesAutoCloseWrapper(t *testing.T) {
	wrapped := NewProxy(outbound.NewAutoCloseProxyAdapter(
		rttAdapter{ProxyAdapter: outbound.NewDirect(), ms: 42, has: true}))
	b, err := wrapped.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if got, ok := m["tide-rtt"]; !ok || got != float64(42) {
		t.Fatalf("tide-rtt = %v (present=%v)，want 42 —— 被包装层挡住了", got, ok)
	}

	// 没有会话（has=false）时不给这个键：观测口不拨号，也不编数字。
	idle := NewProxy(outbound.NewAutoCloseProxyAdapter(
		rttAdapter{ProxyAdapter: outbound.NewDirect()}))
	b, _ = idle.MarshalJSON()
	m = nil
	_ = json.Unmarshal(b, &m)
	if _, ok := m["tide-rtt"]; ok {
		t.Fatal("没有样本时不该出现 tide-rtt")
	}

	// 普通出站（没有这个观测口）照旧没有这个键。
	plain := NewProxy(outbound.NewAutoCloseProxyAdapter(outbound.NewDirect()))
	b, _ = plain.MarshalJSON()
	m = nil
	_ = json.Unmarshal(b, &m)
	if _, ok := m["tide-rtt"]; ok {
		t.Fatal("普通出站不该有 tide-rtt")
	}
}
