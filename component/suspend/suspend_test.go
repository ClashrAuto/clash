package suspend

import (
	"testing"
	"time"
)

// 醒着时永远放行,而且**不记账** —— 记了的话「醒着扫过一轮」会把随后刚睡下的
// 那一轮挡掉,而那一轮恰恰最该做(设备刚睡,健康数据要新)。
func TestAllowFailureSweep_AwakeAlwaysAllowsAndDoesNotRecord(t *testing.T) {
	Resume()
	reset()
	for i := 0; i < 5; i++ {
		if !AllowFailureSweep() {
			t.Fatalf("醒着时第 %d 次被挡了", i+1)
		}
	}
	Suspend()
	defer Resume()
	if !AllowFailureSweep() {
		t.Fatal("刚睡下的第一轮必须放行(醒着那几轮不该记账)")
	}
}

// 挂起态:首轮放行,冷却期内后续一律挡掉 —— 这正是「睡着时后台滴流每次拨号失败
// 都全量重扫 74 个节点」那条风暴路径。
func TestAllowFailureSweep_SuspendedThrottles(t *testing.T) {
	Suspend()
	defer Resume()
	reset()
	if !AllowFailureSweep() {
		t.Fatal("挂起态首轮应放行(死节点仍要能自愈)")
	}
	for i := 0; i < 3; i++ {
		if AllowFailureSweep() {
			t.Fatalf("冷却期内第 %d 次不该放行", i+1)
		}
	}
}

// 冷却期满之后要重新放行 —— 自愈不能被永久挡死。
func TestAllowFailureSweep_ReallowsAfterCooldown(t *testing.T) {
	Suspend()
	defer Resume()
	reset()
	if !AllowFailureSweep() {
		t.Fatal("首轮应放行")
	}
	sweepMu.Lock()
	lastSweep = time.Now().Add(-FailureSweepCooldown - time.Second)
	sweepMu.Unlock()
	if !AllowFailureSweep() {
		t.Fatal("冷却期满后应重新放行")
	}
}

func reset() {
	sweepMu.Lock()
	lastSweep = time.Time{}
	sweepMu.Unlock()
}
