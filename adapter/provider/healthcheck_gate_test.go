package provider

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// probeGate 是「同一节点挂多个组 → 每轮被测 3 次」的唯一修复点，三条语义都要钉住：
//
//   · 并发同 key 合流成一次真实执行（多组同时开测的场景——加载/重载时它们隔着毫秒）；
//   · 窗口内（probeReuseWindow）重复调用复用结果，不再执行；
//   · 不同 key 互不影响——闸门错拦别的节点的话，症状是「有的节点永远测不到」。
func TestProbeGateCoalescesAndReuses(t *testing.T) {
	g := &probeGate{entries: map[string]*probeGateEntry{}}
	var runs atomic.Int32

	// 并发 3 路同 key（模拟 AUTO + 订阅组 + 地区组同时开测）：只真跑 1 次。
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.forKey("node-a|http://u").Do(func() (struct{}, error) {
				runs.Add(1)
				time.Sleep(20 * time.Millisecond) // 撑开并发窗口，让三路真的撞上
				return struct{}{}, nil
			})
		}()
	}
	wg.Wait()
	if n := runs.Load(); n != 1 {
		t.Fatalf("并发同 key 应合流成 1 次执行，实际 %d 次", n)
	}

	// 窗口内再来一次：复用，不执行。
	_, _, shared := g.forKey("node-a|http://u").Do(func() (struct{}, error) {
		runs.Add(1)
		return struct{}{}, nil
	})
	if !shared || runs.Load() != 1 {
		t.Fatalf("窗口内应复用（shared=true 且不再执行），shared=%v runs=%d", shared, runs.Load())
	}

	// 不同 key 不受影响。
	g.forKey("node-b|http://u").Do(func() (struct{}, error) {
		runs.Add(1)
		return struct{}{}, nil
	})
	if runs.Load() != 2 {
		t.Fatalf("不同 key 应独立执行，runs=%d", runs.Load())
	}
}

// 条目清理：超过上限时把闲置的清掉——节点随订阅增删，不清是跨重载的慢泄漏。
func TestProbeGatePrunesIdleEntries(t *testing.T) {
	g := &probeGate{entries: map[string]*probeGateEntry{}}
	for i := 0; i < 1025; i++ {
		g.forKey(string(rune('a'+i%26)) + string(rune('0'+i/26%10)) + string(rune('0'+i/260)) + "|u")
	}
	// 全部标成很久没用过，再插一个触发清理。
	g.mu.Lock()
	for _, e := range g.entries {
		e.lastUsed = time.Now().Add(-time.Hour)
	}
	g.mu.Unlock()
	g.forKey("fresh|u")
	g.mu.Lock()
	n := len(g.entries)
	g.mu.Unlock()
	if n > 2 {
		t.Fatalf("闲置条目应被清掉，剩 %d 个", n)
	}
}
