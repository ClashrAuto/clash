package socks

import (
	"sync"
	"testing"

	"github.com/ClashrAuto/coast/transport/socks5"
)

// 这套测试守的是一个**静默**的失效：翻错了不会崩、不报错，表现是被代理设备的 UDP 流量
// 没有身份（策略不生效、流量记到别人头上），只有拿真机对着 /connections 看才发现。
// 编译期一点信号都没有，所以只能在这里钉住。

func TestUDPUserRegisterLookup(t *testing.T) {
	release := registerUDPUser("127.0.0.1:41234", "dev-aabbccddeeff")
	defer release()

	if got := lookupUDPUser("127.0.0.1:41234"); got != "dev-aabbccddeeff" {
		t.Fatalf("按源地址应查到身份，got %q", got)
	}
	// ★ 只认精确地址：同 IP 不同端口必须查不到。网关会把多台设备的 UDP 都从 127.0.0.1
	//   发出来，这里一旦放宽成按 IP 匹配，就是把甲设备的流量记到乙设备名下。
	if got := lookupUDPUser("127.0.0.1:41235"); got != "" {
		t.Fatalf("端口不同不该命中，got %q", got)
	}
	if got := lookupUDPUser("127.0.0.1"); got != "" {
		t.Fatalf("只有 IP 不该命中，got %q", got)
	}
}

func TestUDPUserReleaseRemoves(t *testing.T) {
	release := registerUDPUser("127.0.0.1:42000", "dev-1")
	if lookupUDPUser("127.0.0.1:42000") == "" {
		t.Fatal("登记后应查得到")
	}
	release()
	if got := lookupUDPUser("127.0.0.1:42000"); got != "" {
		t.Fatalf("注销后应查不到，got %q", got)
	}
	// 幂等：控制连接异常时同一个 release 可能被走到两次，不能把别人的记录减没。
	release()
	release()
	other := registerUDPUser("127.0.0.1:42000", "dev-2")
	defer other()
	if got := lookupUDPUser("127.0.0.1:42000"); got != "dev-2" {
		t.Fatalf("重复 release 不该影响后来的登记，got %q", got)
	}
}

func TestUDPUserRefCount(t *testing.T) {
	r1 := registerUDPUser("127.0.0.1:43000", "dev-x")
	r2 := registerUDPUser("127.0.0.1:43000", "dev-x")
	r1()
	if got := lookupUDPUser("127.0.0.1:43000"); got != "dev-x" {
		t.Fatalf("还有一条引用在，不该被删，got %q", got)
	}
	r2()
	if got := lookupUDPUser("127.0.0.1:43000"); got != "" {
		t.Fatalf("引用清零后应删除，got %q", got)
	}
}

// 同一个来源地址被两个不同身份声明 → 整条作废。归错比归不上更糟：
// 策略会串到别的设备，流量账也会串。
func TestUDPUserConflictInvalidates(t *testing.T) {
	r1 := registerUDPUser("127.0.0.1:44000", "dev-a")
	defer r1()
	r2 := registerUDPUser("127.0.0.1:44000", "dev-b")
	defer r2()
	if got := lookupUDPUser("127.0.0.1:44000"); got != "" {
		t.Fatalf("身份冲突时应作废，got %q", got)
	}
}

// 空参数是常态而非异常：免认证入站没有 user，第三方客户端不声明来源地址。
// 两种都必须是安全的空操作，调用方才能无条件 `defer registerUDPUser(...)()`。
func TestUDPUserEmptyIsNoop(t *testing.T) {
	registerUDPUser("", "dev-a")()
	registerUDPUser("127.0.0.1:45000", "")()
	if got := lookupUDPUser(""); got != "" {
		t.Fatalf("空地址不该命中，got %q", got)
	}
	if got := lookupUDPUser("127.0.0.1:45000"); got != "" {
		t.Fatalf("user 为空时不该留下记录，got %q", got)
	}
}

func TestUDPAssociateKey(t *testing.T) {
	cases := []struct {
		in   string
		want string
		why  string
	}{
		{"127.0.0.1:41234", "127.0.0.1:41234", "客户端声明了具体地址 → 可作键"},
		{"0.0.0.0:0", "", "RFC 允许的「我还不知道」→ 不登记"},
		{"0.0.0.0:41234", "", "通配地址无法逐包比对 → 不登记"},
		{"127.0.0.1:0", "", "端口 0 无法唯一定位会话 → 不登记"},
		{"[::1]:41234", "[::1]:41234", "v6 形态要与 UDPAddr.String() 一致"},
		{"example.com:41234", "", "域名型 ATYP 不是字面地址 → 不登记"},
	}
	for _, c := range cases {
		addr := socks5.ParseAddr(c.in)
		if addr == nil && c.want != "" {
			t.Fatalf("ParseAddr(%q) 解析失败", c.in)
		}
		if got := udpAssociateKey(addr); got != c.want {
			t.Errorf("udpAssociateKey(%q) = %q, want %q（%s）", c.in, got, c.want, c.why)
		}
	}
}

// UDP 中继那条循环是单 goroutine，但登记/注销来自任意多条控制连接的 goroutine。
// 用 -race 跑这个才能证明锁是够的。
func TestUDPUserConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			addr := "127.0.0.1:5" + string(rune('0'+i%10)) + "000"
			release := registerUDPUser(addr, "dev-concurrent")
			_ = lookupUDPUser(addr)
			release()
		}(i)
	}
	wg.Wait()
	if got := lookupUDPUser("127.0.0.1:50000"); got != "" {
		t.Fatalf("全部注销后不该有残留，got %q", got)
	}
}

// ★ 上面那个用例**抓不到撞车路径上的竞态**：它全程只用一个用户名，于是
// registerUDPUser 里 `e.user != user` 永不成立，写 `e.user = ""` 那条根本不执行 ——
// 「读」和「写」从来没有同时发生过，-race 自然什么都看不到。
//
// 这里刻意让两个**不同**身份撞同一个来源地址，把那条写路径拉进并发窗口。
// 2026-08-11 用它跑 -race，当场指到 udpuser.go 的 `e.user = ""`（写）与
// `return e.user`（读，当时在 RUnlock 之后）—— map 被锁保护住了，但 map 存的是指针，
// 锁外读那个指针指向的字段仍然是竞争。修法是把读放回读锁里。
func TestUDPUserConflictConcurrent(t *testing.T) {
	const addr = "127.0.0.1:51000"
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); registerUDPUser(addr, "dev-a")() }()
		go func() { defer wg.Done(); registerUDPUser(addr, "dev-b")() }()
		go func() { defer wg.Done(); _ = lookupUDPUser(addr) }()
	}
	wg.Wait()
	if got := lookupUDPUser(addr); got != "" {
		t.Fatalf("全部注销后不该有残留，got %q", got)
	}
}
