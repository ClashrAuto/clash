package provider

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClashrAuto/coast/common/utils"
	"github.com/ClashrAuto/coast/component/suspend"
	C "github.com/ClashrAuto/coast/constant"
)

// countingProxy 记下被测了几次。没写到的方法调到就是 nil 指针，用例会当场崩，不会悄悄绿。
type countingProxy struct {
	C.Proxy
	name  string
	tests *atomic.Int32
	hold  chan struct{} // 非 nil 时 URLTest 卡到它关闭
}

func (p countingProxy) Name() string { return p.name }

func (p countingProxy) URLTest(context.Context, string, utils.IntRanges[uint16]) (uint16, error) {
	if p.hold != nil {
		<-p.hold
	}
	p.tests.Add(1)
	return 10, nil
}

func (p countingProxy) AliveForTestUrl(string) bool       { return true }
func (p countingProxy) LastDelayForTestUrl(string) uint16 { return 10 }

type onDemandFixture struct {
	pd    *CompatibleProvider
	tests *atomic.Int32
	now   *atomic.Pointer[time.Time]
}

func (f onDemandFixture) advance(d time.Duration) {
	t := f.now.Load().Add(d)
	f.now.Store(&t)
}

// clearReuse 腾掉按真实时钟的短期复用（节点 10 秒、组 1 秒），这里走的是假钟。
func (f onDemandFixture) clearReuse(node string) {
	sharedProbeGate.mu.Lock()
	delete(sharedProbeGate.entries, node+"|http://example.invalid/")
	sharedProbeGate.mu.Unlock()
	f.pd.healthCheck.singleDo.Reset()
}

func newCounting(t *testing.T, group, node string, interval uint, tests *atomic.Int32, hold chan struct{}) *CompatibleProvider {
	t.Helper()
	ps := []C.Proxy{countingProxy{name: node, tests: tests, hold: hold}}
	pd, err := NewCompatibleProvider(group, ps, NewHealthCheck(ps, "http://example.invalid/", 1000, interval, false, nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pd.Close() })
	return pd
}

// newOnDemandFixture 建一个周期 60 秒的自动组。name 要各用例不同：探测闸门按节点名复用 10 秒内的结果。
func newOnDemandFixture(t *testing.T, name string, afterIdle func()) onDemandFixture {
	t.Helper()
	now := new(atomic.Pointer[time.Time])
	start := time.Unix(1_800_000_000, 0)
	now.Store(&start)
	origNow := onDemandNow
	onDemandNow = func() time.Time { return *now.Load() }
	SetOnDemandHealthCheck(true, afterIdle)
	ForgetHealthChecks()
	t.Cleanup(func() {
		SetOnDemandHealthCheck(false, nil)
		ForgetHealthChecks()
		onDemandNow = origNow
	})
	tests := new(atomic.Int32)
	return onDemandFixture{pd: newCounting(t, name, name+"-node", 60, tests, nil), tests: tests, now: now}
}

func waitCount(t *testing.T, c *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for c.Load() < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := c.Load(); got != want {
		t.Fatalf("应为 %d，实际 %d", want, got)
	}
}

func staysAt(t *testing.T, c *atomic.Int32, want int32, what string) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	if got := c.Load(); got != want {
		t.Fatalf("%s：应为 %d，实际 %d", what, want, got)
	}
}

// 手机上最要紧的一条：加载配置不测速。这曾是「隧道每重启一次就全量握手一轮」的来源。
func TestOnDemandLoadDoesNotProbe(t *testing.T) {
	f := newOnDemandFixture(t, "load", nil)
	if err := f.pd.Initial(); err != nil {
		t.Fatal(err)
	}
	f.pd.setProxies(f.pd.Proxies()) // 提供者更新节点时的那条路也不许开测
	staysAt(t, f.tests, 0, "没人用的时候不该测")
}

// 用到才测：第一次用测一轮，有效期内不再测，过期后的下一次使用再测一轮。
func TestOnDemandProbesOnUseOncePerAge(t *testing.T) {
	f := newOnDemandFixture(t, "use", nil)
	f.pd.Touch()
	waitCount(t, f.tests, 1)
	time.Sleep(20 * time.Millisecond) // 等 finishOnDemandCheck 记下完成时刻

	f.advance(onDemandMaxAge - time.Second)
	f.pd.Touch()
	staysAt(t, f.tests, 1, "有效期内不该再测")

	f.advance(2 * time.Second)
	f.clearReuse("use-node")
	f.pd.Touch()
	waitCount(t, f.tests, 2)
}

// 系统时间被往回调过：那条记录不可信，按过期算，不能一直判「刚测过」。
func TestOnDemandTreatsFutureRecordAsExpired(t *testing.T) {
	f := newOnDemandFixture(t, "clock", nil)
	f.pd.Touch()
	waitCount(t, f.tests, 1)
	time.Sleep(20 * time.Millisecond)
	f.advance(-time.Hour)
	f.clearReuse("clock-node")
	f.pd.Touch()
	waitCount(t, f.tests, 2)
}

// 一波连接同时摸到，只开一轮。
func TestOnDemandBurstOfTouchesProbesOnce(t *testing.T) {
	f := newOnDemandFixture(t, "burst", nil)
	for i := 0; i < 50; i++ {
		f.pd.Touch()
	}
	waitCount(t, f.tests, 1)
	staysAt(t, f.tests, 1, "一波连接应只测一轮")
}

// 锁屏听歌时流量一直在走，但锁屏时不测。
func TestOnDemandSkipsWhileSuspended(t *testing.T) {
	f := newOnDemandFixture(t, "held", nil)
	suspend.Suspend()
	defer suspend.Resume()
	f.pd.Touch()
	staysAt(t, f.tests, 0, "挂起时不该测")
}

// 不是自动组（select）不测：它没有测速可言，开测只是白白把节点握手一遍。
func TestOnDemandIgnoresNonAutoGroups(t *testing.T) {
	newOnDemandFixture(t, "unused", nil)
	tests := new(atomic.Int32)
	pd := newCounting(t, "sel", "sel-node", 0, tests, nil)
	pd.Touch()
	staysAt(t, tests, 0, "select 组不该测")
}

// 失败触发的体检保留，而且算一次测过（否则紧接着的使用又全量测一轮）。
func TestOnDemandFailureSweepStillRunsAndCounts(t *testing.T) {
	f := newOnDemandFixture(t, "sweep", nil)
	f.pd.HealthCheck()
	waitCount(t, f.tests, 1)
	f.pd.Touch()
	staysAt(t, f.tests, 1, "刚体检过不该再测")
}

// 组重建之后「上次测过」还在：iOS 每次解锁都要重建一遍组，留在对象上就是每次解锁全量测。
func TestOnDemandAgeSurvivesProviderRebuild(t *testing.T) {
	f := newOnDemandFixture(t, "rebuild", nil)
	f.pd.Touch()
	waitCount(t, f.tests, 1)
	time.Sleep(20 * time.Millisecond)

	tests := new(atomic.Int32)
	pd2 := newCounting(t, "rebuild", "rebuild-node2", 60, tests, nil)
	pd2.Touch()
	staysAt(t, tests, 0, "同名的组重建之后不该重测")

	ForgetHealthChecks() // 节点集合变了
	pd2.Touch()
	waitCount(t, tests, 1)
}

// 清长连接要等全部测完。只等自己这一组的话，会把别的组正在用的连接掐掉，活节点被测成死的。
func TestOnDemandAfterIdleWaitsForAllChecks(t *testing.T) {
	var after atomic.Int32
	f := newOnDemandFixture(t, "first", func() { after.Add(1) })

	hold := make(chan struct{})
	tests := new(atomic.Int32)
	slow := newCounting(t, "slow", "slow-node", 60, tests, hold)

	slow.Touch()
	f.pd.Touch()
	waitCount(t, f.tests, 1)
	staysAt(t, &after, 0, "还有一组没测完，不该清长连接")
	close(hold)
	waitCount(t, &after, 1)
}
