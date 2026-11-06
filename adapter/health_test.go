package adapter

import (
	"testing"
	"time"

	"github.com/ClashrAuto/coast/common/atomic"
	"github.com/ClashrAuto/coast/common/queue"
	C "github.com/ClashrAuto/coast/constant"
)

// 测速结果搬到新对象上之后，读出来要和旧对象一样（活没活、最后延迟、按 URL 的那份都算）。
func TestHealthSurvivesRebuild(t *testing.T) {
	const url = "http://example.invalid/"
	old := NewProxy(nil)
	old.alive.Store(false)
	old.history.Put(C.DelayHistory{Time: time.Unix(1, 0), Delay: 120})
	state := &internalProxyState{
		alive:   atomic.NewBool(true),
		history: queue.New[C.DelayHistory](defaultHistoriesNum),
	}
	state.history.Put(C.DelayHistory{Time: time.Unix(2, 0), Delay: 80})
	old.extra.Store(url, state)

	fresh := NewProxy(nil)
	fresh.RestoreHealth(old.Health())

	if fresh.alive.Load() {
		t.Fatal("死活状态没搬过来")
	}
	if got := fresh.DelayHistory(); len(got) != 1 || got[0].Delay != 120 {
		t.Fatalf("总历史没搬过来：%+v", got)
	}
	if !fresh.AliveForTestUrl(url) {
		t.Fatal("按 URL 的存活状态没搬过来")
	}
	if got := fresh.LastDelayForTestUrl(url); got != 80 {
		t.Fatalf("按 URL 的延迟没搬过来：%d", got)
	}
}

// 没测过的节点搬过去还是「没测过」，不能因为搬运变成别的状态。
func TestHealthOfUntestedProxyStaysUntested(t *testing.T) {
	fresh := NewProxy(nil)
	fresh.RestoreHealth(NewProxy(nil).Health())
	if !fresh.alive.Load() || len(fresh.DelayHistory()) != 0 {
		t.Fatalf("没测过的节点状态变了：alive=%v history=%d", fresh.alive.Load(), len(fresh.DelayHistory()))
	}
}
