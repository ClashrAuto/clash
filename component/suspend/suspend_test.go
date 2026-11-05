package suspend

import (
	"strings"
	"testing"
	"time"
)

// 醒着时一律放行 —— 上游语义不变（快速自愈优先）。
func TestAllowFailureSweep_AwakeAllows(t *testing.T) {
	Resume()
	for i := 0; i < 5; i++ {
		if !AllowFailureSweep() {
			t.Fatalf("醒着时第 %d 次被挡了", i+1)
		}
	}
}

// 挂起态一次都不做 —— 这正是「睡着时后台滴流每次拨号失败都全量重扫上百个节点」
// 那条风暴路径（2026-09-05 用户拍板：连当前在用的节点也不必测）。
func TestAllowFailureSweep_SuspendedNeverAllows(t *testing.T) {
	Suspend()
	defer Resume()
	for i := 0; i < 5; i++ {
		if AllowFailureSweep() {
			t.Fatalf("挂起态第 %d 次不该放行", i+1)
		}
	}
}

// 醒来立刻恢复放行 —— 自愈靠的是 Resume 那一下（它还会跑补跑回调），
// 不是睡着时硬扫。
func TestAllowFailureSweep_ResumeRestores(t *testing.T) {
	Suspend()
	if AllowFailureSweep() {
		t.Fatal("挂起态不该放行")
	}
	Resume()
	if !AllowFailureSweep() {
		t.Fatal("恢复后应立刻放行")
	}
}

// 「睡了一觉」的两个边沿（2026-11-05「iOS 解锁后断网」的根因修复靠它）：
// 重复挂起/醒来只算一次边沿；醒来时先通知 SleepWatcher，再跑补跑回调——
// 顺序反了的话，补测会先走进睡前留下的死连接、把节点测死，对账再丢连接已经晚了。
func TestSleepWatcherEdgesAndOrder(t *testing.T) {
	Resume() // 归位：醒着
	var events []string
	key := new(int)
	RegisterSleepWatcher(key, SleepWatcher{
		OnSleep: func() { events = append(events, "sleep") },
		OnWake:  func(time.Duration) { events = append(events, "wake") },
	})
	defer UnregisterSleepWatcher(key)
	hookKey := new(int)
	RegisterResumeHook(hookKey, func() { events = append(events, "hook") })
	defer UnregisterResumeHook(hookKey)

	Suspend()
	Suspend() // iOS 有好几条路会喊挂起，重复喊不是新的一觉
	Resume()
	Resume() // 醒着时多喊一次：不是新的醒来边沿，补跑回调照旧跑（原有语义）
	want := []string{"sleep", "wake", "hook", "hook"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("边沿/顺序不对：得到 %v，应为 %v", events, want)
	}
}

// 「睡了多久」按墙钟量：单调钟在设备睡眠时停走，量出来的会远小于真实时长，
// 而对端与 NAT 的空闲超时是按墙钟走的。
func TestSleptIsWallClock(t *testing.T) {
	Resume()
	Suspend()
	if s := suspendedAt.Load().String(); strings.Contains(s, "m=") {
		Resume()
		t.Fatalf("挂起时刻带着单调读数（%s）—— 设备睡眠期间它不走", s)
	}
	var got time.Duration
	key := new(int)
	RegisterSleepWatcher(key, SleepWatcher{OnWake: func(s time.Duration) { got = s }})
	defer UnregisterSleepWatcher(key)
	suspendedAt.Store(time.Now().Round(0).Add(-10 * time.Minute)) // 等价于墙钟上睡了 10 分钟
	Resume()
	if got < 10*time.Minute || got > 10*time.Minute+5*time.Second {
		t.Fatalf("应量出约 10 分钟，得到 %v", got)
	}
}
