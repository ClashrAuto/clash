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
