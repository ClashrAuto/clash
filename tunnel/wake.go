package tunnel

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ClashrAuto/coast/component/suspend"
	C "github.com/ClashrAuto/coast/constant"
	"github.com/ClashrAuto/coast/log"
	"github.com/ClashrAuto/coast/tunnel/statistic"
)

// 醒来对账：把熬不过这一觉的上游连接丢掉（Coast 自有，2026-11-05「iOS 锁屏后亮屏断网」的根因修复）。
//
// ★★★ 根因：设备睡着时进程被冻结，长连接的保活包一个都发不出去；对端与 NAT 按墙钟计时，
// 空闲超时一到就把状态丢了（hysteria2 的 QUIC 空闲超时 30s、tide 服务端的会话宽限 120s、
// NAT 的 UDP 映射常见 30s）。本进程这一侧却靠 Go 的单调钟判断连接还活不活——设备睡眠时
// 单调钟停走——于是醒来时还把这些连接当活的：多路复用/QUIC/tide 会话照旧复用，睡前的
// 中继连接照旧挂着。解锁后第一波流量全部走进死连接，各自等到本地超时才失败；健康检查的
// 补测也走进去，把节点测死，失败又触发全量体检。别的 app 在这段里反复重试，Coast 自己在
// 反复全量握手——两边的耗电都从这里来。
//
// 修法是在「挂起 → 醒来」的边沿上对账，判据只有一条：**这一觉里一个字节都没走过**。
//   - 睡前就在、这一觉里上下行都没动过的中继连接 → 关掉。它的上游多半已死；就算还活着，
//     关一个空闲连接的代价也只是 app 下次用时重连一次。睡眠期间新建的连接不碰——解锁那一刻
//     正在握手的连接就在其中。
//   - 出站：这一觉里它承载的连接没有任何一条走过字节 → 重置它攥着的长连接（QUIC 连接、
//     多路复用会话、tide 会话），下一次拨号重新握手（各出站的 ResetTransport）。锁屏听歌这类
//     一直在走流量的，保活也一直在跑，不碰。逐连接拨号的协议没有长连接可重置，它们的死连接
//     由上一条带走。
//   - 睡得比 [wakeReconcileMinSleep] 短 → 什么都不做。
//
// 「走过字节」对睡前就在的连接上下行都算：宁可漏丢一条死连接（它会在自己的超时里收场），
// 也绝不掐断一条正在走流量的活连接（比如锁屏时只上传、暂时没有回包的备份）。
// 对**睡眠期间新建**的连接只认「收到过字节」：iOS 解锁时路由先回来、内核后醒，这几百毫秒里
// app 新建的连接正往死长连接里写——写得进去不代表对面还在，拿它当「出站还活着」的证据，
// 那条死长连接就永远重置不掉。锁屏听歌中途重连的那种新连接一直有回包，照样算忙。
//
// ★ 对账在补跑健康检查**之前**（suspend.Resume 先通知 SleepWatcher）：补测走的就是新连接。
// ★ 在这里做而不是让各平台自己调：Android（REST /suspend）、Qt 桌面（同）、macOS/iOS（C 接口）
// 喊的都是同一对 Suspend/Resume，四条线一起修好，平台侧不必各自记得。

// wakeReconcileMinSleep 低于它的一觉不对账：上面列的那些空闲超时里最短的也有 30 秒，
// 更短的一觉里没有任何上游状态会过期，丢掉它们只会白白多握一次手。
const wakeReconcileMinSleep = 30 * time.Second

var (
	wakeMu sync.Mutex
	// 进入挂起那一刻每条中继连接的累计字节（上行 + 下行）。醒来时用掉即清空。
	wakeBefore map[string]int64
)

func init() {
	suspend.RegisterSleepWatcher(&wakeMu, suspend.SleepWatcher{
		OnSleep: onSleepSnapshot,
		OnWake:  onWakeReconcile,
	})
}

func onSleepSnapshot() {
	before := map[string]int64{}
	statistic.DefaultManager.Range(func(c statistic.Tracker) bool {
		before[c.ID()] = trackedBytes(c.Info())
		return true
	})
	wakeMu.Lock()
	wakeBefore = before
	wakeMu.Unlock()
}

func onWakeReconcile(slept time.Duration) {
	wakeMu.Lock()
	before := wakeBefore
	wakeBefore = nil
	wakeMu.Unlock()
	if slept < wakeReconcileMinSleep || before == nil {
		return
	}
	var conns []wakeTracker
	statistic.DefaultManager.Range(func(c statistic.Tracker) bool {
		conns = append(conns, c)
		return true
	})
	closed, reset := reconcileWake(before, conns, wakeProxies())
	// ★ DNS 上游的长连接（DoH/DoT）是同一类状态，但这里**不**调 resolver.ResetConnection：
	//   各平台醒来时本来就重建它（iOS CoastResetDNS、macOS 等网络就绪后 resetDNS、Android
	//   /dns/reset），而那个函数在 DNS 还没配置时会对 nil 的 SystemResolver 解引用——
	//   放进这条每次醒来都走的路径，就多了一条让隧道进程崩掉的路（单测第一次跑就撞上了）。
	// ★ warning 级：隧道形态的日志压在 warning，这是「醒来时丢了什么」唯一持久可见的痕迹；
	//   一觉只打一行。
	log.Warnln("[Wake] slept %s: closed %d connection(s) idle across the sleep, reset transports of %d idle outbound(s)%s",
		slept.Round(time.Second), closed, len(reset), namesSuffix(reset))
}

// wakeTracker 是对账要用到的那一点中继连接接口（statistic.Tracker 满足它；单测用假的）。
type wakeTracker interface {
	ID() string
	Close() error
	Info() *statistic.TrackerInfo
}

func trackedBytes(info *statistic.TrackerInfo) int64 {
	return info.UploadTotal.Load() + info.DownloadTotal.Load()
}

// wakeSample 是醒来那一刻一条中继连接的样本。
type wakeSample struct {
	id    string
	bytes int64 // 上行 + 下行累计
	down  int64 // 下行累计（收到过的字节）
	chain C.Chain
}

// wakePlan 是一次对账的决定。
type wakePlan struct {
	closeIDs []string        // 睡前就在、这一觉里一个字节都没走过的连接
	busy     map[string]bool // 这一觉里走过字节的出站（链上每一个名字都算）
}

// planWake 是对账的判定，纯函数。before 是挂起那一刻的字节数。
func planWake(before map[string]int64, now []wakeSample) wakePlan {
	plan := wakePlan{busy: map[string]bool{}}
	for _, c := range now {
		prev, existed := before[c.id]
		switch {
		case existed && c.bytes == prev:
			plan.closeIDs = append(plan.closeIDs, c.id)
		case existed, c.down > 0:
			// 睡前就在、这一觉里动过（任一方向）；或者睡眠期间新建、收到过字节。
			for _, name := range c.chain {
				plan.busy[name] = true
			}
		}
		// 睡眠期间新建、一个字节都没收到的：不关（可能正在握手），也不算出站活着
		// （可能正往死长连接里写——见文件头）。
	}
	return plan
}

// ResetIdleTransports 丢掉**此刻没有任何中继连接在用**的出站的长连接，返回重置了几个出站
// （Coast 自有，平台钉的首选网口变了时、按需测速结束时调）。
//
// 换口只影响新拨号，已有的长连接留在旧口上不会自己回来 —— 从蜂窝回到 Wi-Fi 之后，
// 蜂窝上的 tide 路径照样活着、探测照发，直到它自己死掉（可能永远不死）。
//
// 为什么只动空闲的：tide 的卖点就是切网时流不断（宽限期内重连续上），正在用的会话
// 硬关掉等于把这个能力废了。正在用的留着，等它用完。
func ResetIdleTransports() int {
	busy := map[string]bool{}
	statistic.DefaultManager.Range(func(c statistic.Tracker) bool {
		for _, name := range c.Info().Chain {
			busy[name] = true
		}
		return true
	})
	return len(resetTransports(wakeProxies(), busy))
}

// resetTransports 重置 proxies 里除 busy 之外所有出站的长连接，返回重置了谁（已排序）。
func resetTransports(proxies map[string]C.Proxy, busy map[string]bool) []string {
	var reset []string
	for name, p := range proxies {
		if busy[name] {
			continue
		}
		if resetTransport(p) {
			reset = append(reset, name)
		}
	}
	sort.Strings(reset)
	return reset
}

// reconcileWake 执行对账：关掉空闲的中继连接，重置空闲出站的长连接。返回关了几条、重置了谁。
func reconcileWake(before map[string]int64, conns []wakeTracker, proxies map[string]C.Proxy) (closed int, reset []string) {
	samples := make([]wakeSample, 0, len(conns))
	byID := make(map[string]wakeTracker, len(conns))
	for _, c := range conns {
		info := c.Info()
		samples = append(samples, wakeSample{id: c.ID(), bytes: trackedBytes(info),
			down: info.DownloadTotal.Load(), chain: info.Chain})
		byID[c.ID()] = c
	}
	plan := planWake(before, samples)
	for _, id := range plan.closeIDs {
		_ = byID[id].Close()
		closed++
	}
	return closed, resetTransports(proxies, plan.busy)
}

// resetTransport 沿包装层（adapter.Proxy → autoClose → …）找到实现了 ResetTransport 的那一层
// 并调用它。返回 false = 这个出站没有可重置的长连接（逐连接拨号的协议、代理组）。
//
// ★ 只靠接口穿透，不 import adapter：adapter/outbound → dns → tunnel 会成环。
// 8 层上限只是防御，正常只有一两层（与 adapter.pathRTTOf 同一个写法）。
func resetTransport(p C.Proxy) bool {
	var a C.ProxyAdapter = p.Adapter()
	for i := 0; i < 8 && a != nil; i++ {
		if r, ok := a.(interface{ ResetTransport() }); ok {
			r.ResetTransport()
			return true
		}
		u, ok := a.(interface{ Wrapped() C.ProxyAdapter })
		if !ok {
			return false
		}
		a = u.Wrapped()
	}
	return false
}

// wakeProxies 汇总配置里的出站与各 provider 里的出站（同名去重）。
func wakeProxies() map[string]C.Proxy {
	configMux.RLock()
	defer configMux.RUnlock()
	out := make(map[string]C.Proxy, len(proxies))
	for name, p := range proxies {
		out[name] = p
	}
	for _, pd := range providers {
		for _, p := range pd.Proxies() {
			out[p.Name()] = p
		}
	}
	return out
}

func namesSuffix(names []string) string {
	if len(names) == 0 {
		return ""
	}
	const show = 5
	if len(names) <= show {
		return ": " + strings.Join(names, ", ")
	}
	return ": " + strings.Join(names[:show], ", ") + ", …"
}
