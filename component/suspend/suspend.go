// Package suspend —— 「设备睡了，周期性后台活动也睡」的全局开关（Coast 自有，上游没有）。
//
// ★★★ 为什么需要：url-test 组默认每 60 秒把**全部**节点真实探测一遍（TLS/QUIC 握手打到
//
//	真服务器），而 `lazy` 挡不住它 —— `lastTouch` 在每次经组拨号时都被摸一下，手机上
//	永远有后台滴流（推送、遥测），于是探测 24 小时不停。蜂窝射频每次唤起要拖 10~15 秒
//	高功耗尾巴，60 秒一轮 = 射频每小时 10~15 分钟高功耗，仅此一项每小时烧掉百分之几的电。
//	这正是「挂着 VPN 特别耗电」的主因（2026-08-22 逐环核实：seed interval=60、
//	urltest.Touch 每拨必摸、FINAL 兜底指向 PROXY/AUTO）。
//
// 语义：
//   - Suspend()：**跳过**周期性活动（健康检查的周期轮、statistic 每秒归零）。
//     不停正在跑的流量 —— 隧道照常转发，停的只是我们自己主动发起的探测。
//   - Resume()：恢复，并**立刻**补一轮最近被用过的健康检查 —— 设备醒来第一次用之前，
//     健康数据就是新的，不用等下一个 interval。
//
// 谁来调：
//   - iOS 隧道进程走 C 接口（`libcoast.go` 的 CoastSuspend/CoastResume，
//     由 NEPacketTunnelProvider 的 sleep()/wake() 触发）；
//   - Android 走 REST（`hub/route` 的 /suspend，由息屏/亮屏触发）——
//     Android 的核心是子进程，REST 是唯一的运行时通道。
//
// ★ 实现是**跳过**而不是停/建 ticker：ticker 继续走（一次空 tick 的代价 ≈ 一次唤醒，
//
//	相比停掉省的几十条真实网络连接可以忽略），换来的是零竞态 —— 不存在
//	「Suspend 与 ticker 重建赛跑」这类窗口。
package suspend

import (
	"sync"

	"github.com/ClashrAuto/coast/common/atomic"
)

var (
	suspended atomic.Bool

	mu sync.Mutex
	// Resume 时要补跑的回调（健康检查注册进来）。key 用注册方给的指针身份。
	resumeHooks = map[any]func(){}
)

// Suspended 报告当前是否处于挂起态。周期性活动在自己的 tick 里查它。
func Suspended() bool { return suspended.Load() }

// Suspend 进入挂起态。幂等。
func Suspend() { suspended.Store(true) }

// Resume 退出挂起态，并同步调用全部已注册的补跑回调。幂等（未挂起时只跑回调也无害，
// 回调自己带「最近用过才跑」的判断）。
func Resume() {
	suspended.Store(false)
	mu.Lock()
	hooks := make([]func(), 0, len(resumeHooks))
	for _, f := range resumeHooks {
		hooks = append(hooks, f)
	}
	mu.Unlock()
	// ★ 锁外调用：回调里会起 goroutine 去做真实网络探测，攥着锁调会把注册/注销挡住。
	for _, f := range hooks {
		f()
	}
}

// AllowFailureSweep 报告「拨号失败触发的全量体检」此刻该不该放行。
//
// ★★★ 为什么需要它（2026-09-05 真机对账）：`Suspend()` 只挡周期轮，而
//   `GroupBase.onDialFailed → healthCheck()` 那条路**不经过任何闸门** ——
//   更狠的是它对 `connection refused` 特判成「一次失败就立刻全量重扫」，
//   连连败计数都跳过。手机睡着时后台滴流不断，拨号失败也就不断，于是
//   **挂起态下照样每分钟全量扫一遍 74 个节点**：实测息屏时仍有 1.35 次拨号/秒、
//   蜂窝 24.6 包/秒（关掉 VPN 的基线是 2.2），射频整夜睡不下去。
//   用户连报三天「Android 还是很耗电」，症结在这里，而 `/suspend` 看起来是好的
//   （核心确实收下了、周期轮确实停了），所以从外面完全看不出来。
//
// ★★ **挂起态一律不做，一次都不做**（2026-09-05 用户拍板：「不做全量重扫，
//   连当前在用的节点也不需要」）。先前那版留了 10 分钟冷却，想保住「睡着时死节点
//   仍能自愈」——实测那一轮摊下来仍有约 2 包/秒，而它买到的东西并不值：
//   · 设备一醒，[Resume] 立刻跑注册好的补跑回调（健康检查就在里面），
//     健康数据在用户真正用到之前就是新的；
//   · 睡着时「自愈」能救的只有「后台推送连接断了要重连」，而那条连接本身断了
//     就会由 app 自己重试拨号，拨号会走当前节点——真死了也只是这一条失败，
//     不需要为它把上百个节点全握一遍手。
//   代价说清楚：一觉之间某个节点死掉的话，靠它的后台连接要等到亮屏才恢复。
//
// ★ 非挂起态一律放行 —— 醒着时保持上游语义不变（快速自愈优先）。
func AllowFailureSweep() bool { return !suspended.Load() }

// RegisterResumeHook 注册「恢复时补跑」的回调；key 用调用方自己的指针，便于注销。
// 健康检查在 NewHealthCheck 注册、close 时注销 —— 配置热重载会成对地走这两步。
func RegisterResumeHook(key any, f func()) {
	mu.Lock()
	defer mu.Unlock()
	resumeHooks[key] = f
}

// UnregisterResumeHook 注销。对未注册的 key 是空操作。
func UnregisterResumeHook(key any) {
	mu.Lock()
	defer mu.Unlock()
	delete(resumeHooks, key)
}
