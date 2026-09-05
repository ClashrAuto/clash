package suspend

import "testing"

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
