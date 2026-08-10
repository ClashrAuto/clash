package process

import (
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 刷新必须合并：一波并发未命中只该触发**一次**网卡枚举。
// 用一个计数器替换真正的枚举，数一数一波并发未命中会触发多少次枚举。
func TestRefreshHerd(t *testing.T) {
	var enumerations int64
	orig := enumerate
	enumerate = func() map[netip.Addr]struct{} {
		atomic.AddInt64(&enumerations, 1)
		time.Sleep(2 * time.Millisecond) // 枚举网卡是有代价的
		return map[netip.Addr]struct{}{netip.MustParseAddr("10.0.0.1"): {}}
	}
	defer func() { enumerate = orig }()

	// 先做一次，把缓存建起来并把时间戳推到「已过期」
	isLocalAddr(netip.MustParseAddr("10.0.0.1"))
	localAddrs.mu.Lock()
	localAddrs.refresh = time.Now().Add(-time.Hour)
	localAddrs.mu.Unlock()
	atomic.StoreInt64(&enumerations, 0)

	// 一波并发未命中（网关场景：源地址都是局域网别的机器）
	var wg sync.WaitGroup
	miss := netip.MustParseAddr("192.168.99.99")
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); isLocalAddr(miss) }()
	}
	wg.Wait()
	n := atomic.LoadInt64(&enumerations)
	t.Logf("64 条并发未命中 → 触发了 %d 次网卡枚举", n)
	if n > 4 {
		t.Fatalf("刷新风暴：期望合并成 1 次（容忍 <=4），实际 %d 次", n)
	}
}
