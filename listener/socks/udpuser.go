package socks

import "sync"

// SOCKS5 的 UDP 中继是一个**共享套接字**：数据报直接打到中继端口上，跟当初做过认证的那条
// TCP 控制连接之间没有任何天然关联。于是 UDP 流量进到核心就是**没有身份**的 ——
// `IN-USER` 规则永不命中，`/connections` 里 `inboundUser` 恒为空。对上层的表现是：
// 一台被代理设备的 QUIC / HTTP-3 流量（YouTube 就是典型）既拿不到 per-user 策略，
// 流量还会被记到别人（通常是「本机」）头上。TCP 没这个问题 —— 见 tcp.go 的 WithInUser。
//
// ★ 补法用的是 RFC 1928 里**本来就有**的字段，不扩展协议：UDP ASSOCIATE 请求的
// DST.ADDR/DST.PORT 语义就是「客户端将从这个地址发 UDP」。客户端声明了具体地址，就在这里
// 登记 addr → user；数据报按源地址反查。登记的寿命跟着那条控制连接（RFC 要求它在整个会话
// 期间保持打开，正好是天然的生命周期锚点）。
//
// ★★ **只做精确匹配（ip:port），绝不退化到只按 IP 匹配。** 用户态网关会把多台设备的 UDP
// 统统从 127.0.0.1 发出来，只按 IP 匹配等于把甲设备的流量记到乙设备名下 —— **归错比归不上
// 更糟**（策略会串，账也会串）。同理，同一个来源地址被两个不同身份声明时整条记录作废，
// 见 registerUDPUser。
//
// 客户端没声明地址（DST 填 0.0.0.0:0，这是绝大多数第三方 SOCKS 客户端的写法）时不登记，
// 行为与本改动之前完全一致 —— 拿不到身份，但也绝不会张冠李戴。

type udpUserEntry struct {
	user string
	refs int
}

var (
	udpUserMu sync.RWMutex
	udpUsers  = make(map[string]*udpUserEntry)
)

// registerUDPUser 登记「从 addr 发来的 UDP 数据报属于 user」，返回注销用的闭包。
//
// addr 或 user 为空（没声明地址 / 免认证入站）时不登记，返回的 release 是空操作 ——
// 调用方可以无条件 `defer registerUDPUser(...)()`，不必自己判空。
// release 幂等，重复调用只生效一次。
func registerUDPUser(addr string, user string) (release func()) {
	if addr == "" || user == "" {
		return func() {}
	}

	udpUserMu.Lock()
	e, ok := udpUsers[addr]
	if !ok {
		e = &udpUserEntry{user: user}
		udpUsers[addr] = e
	} else if e.user != user {
		// 同一个来源地址被两个不同身份声明：无法判定是谁的，整条作废（空串 = 查不到）。
		// 正常客户端不会撞车 —— 一个 UDP 端口同一时刻只可能被一个进程独占。
		e.user = ""
	}
	e.refs++
	udpUserMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			udpUserMu.Lock()
			// 按 key 重新取一次，而不是用捕获的 e：万一这条 key 已经被别人删掉重建，
			// 减的也该是**当前**那条的引用计数。
			if cur, ok := udpUsers[addr]; ok {
				cur.refs--
				if cur.refs <= 0 {
					delete(udpUsers, addr)
				}
			}
			udpUserMu.Unlock()
		})
	}
}

// lookupUDPUser 按数据报的源地址反查身份，查不到返回空串。
//
// ★ **`e.user` 必须在读锁**里读完。原来的写法是先 RUnlock 再 `return e.user` —— map 本身
//   是保护住了，但 map 存的是**指针**，锁外读那个指针指向的字段与 registerUDPUser 里
//   `e.user = ""`（撞车作废那条，持写锁）构成数据竞争。go test -race 直接指到这两行。
//   后果不是"偶尔读到旧值"那么温和：Go 的 string 是 (ptr,len) 两个字，撕裂读可能拿到
//   新 ptr 配旧 len，越界读 → 核心在 **UDP 数据面**上 panic。而这个函数是**每个数据报
//   调一次**的，暴露面按包计。
func lookupUDPUser(addr string) string {
	if addr == "" {
		return ""
	}
	udpUserMu.RLock()
	defer udpUserMu.RUnlock()
	e, ok := udpUsers[addr]
	if !ok {
		return ""
	}
	return e.user
}
