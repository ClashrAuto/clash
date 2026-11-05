package tunnel

import (
	"strings"
	"testing"
	"time"

	C "github.com/ClashrAuto/coast/constant"
	"github.com/ClashrAuto/coast/tunnel/statistic"
)

// ── 假件：只实现对账真正会调到的那几个方法，其余靠嵌入的 nil 接口占位 ──

type fakeConn struct {
	C.Connection // 不会被调到
	id           string
	info         *statistic.TrackerInfo
	closed       bool
}

func (f *fakeConn) ID() string                   { return f.id }
func (f *fakeConn) Close() error                 { f.closed = true; return nil }
func (f *fakeConn) Info() *statistic.TrackerInfo { return f.info }

func newFakeConn(id string, bytes int64, chain ...string) *fakeConn {
	info := &statistic.TrackerInfo{Chain: C.Chain(chain)}
	info.DownloadTotal.Add(bytes)
	return &fakeConn{id: id, info: info}
}

type fakeProxy struct {
	C.Proxy // 只有 Adapter() 会被调到
	adapter C.ProxyAdapter
}

func (f fakeProxy) Adapter() C.ProxyAdapter { return f.adapter }

// 有长连接可重置的出站（QUIC / 多路复用 / tide 那一类）。
type resettable struct {
	C.ProxyAdapter
	resets *int
}

func (r resettable) ResetTransport() { *r.resets++ }

// parser 给每个出站套的那层壳（autoClose），只能经 Wrapped 穿过去。
type shell struct {
	C.ProxyAdapter
	inner C.ProxyAdapter
}

func (s shell) Wrapped() C.ProxyAdapter { return s.inner }

// 没有长连接的出站（逐连接拨号的协议、代理组）。
type plainAdapter struct{ C.ProxyAdapter }

// ── 判定 ──

func TestPlanWake(t *testing.T) {
	before := map[string]int64{"idle": 100, "busy": 100, "gone": 5}
	plan := planWake(before, []wakeSample{
		{id: "idle", bytes: 100, down: 60, chain: C.Chain{"A", "AUTO"}},     // 睡前就在、一个字节没动 → 关
		{id: "busy", bytes: 150, down: 60, chain: C.Chain{"B", "AUTO"}},     // 走过字节 → 留，B 算忙
		{id: "new-idle", bytes: 0, chain: C.Chain{"C"}},                     // 睡眠期间新建、还没字节 → 不关、不算忙
		{id: "new-busy", bytes: 10, down: 10, chain: C.Chain{"D", "PROXY"}}, // 睡眠期间新建、收到过字节 → D 算忙
		{id: "new-write", bytes: 300, down: 0, chain: C.Chain{"E"}},         // 睡眠期间新建、只写没收到 → 不算忙
	})
	if strings.Join(plan.closeIDs, ",") != "idle" {
		t.Fatalf("只该关「睡前就在且没动过」的那条，得到 %v", plan.closeIDs)
	}
	for _, name := range []string{"B", "AUTO", "D", "PROXY"} {
		if !plan.busy[name] {
			t.Fatalf("%s 这一觉里走过字节，应算忙", name)
		}
	}
	for _, name := range []string{"A", "C"} {
		if plan.busy[name] {
			t.Fatalf("%s 这一觉里一个字节都没走过，不该算忙", name)
		}
	}
	// ★ iOS 解锁时路由先回来、内核后醒：这中间新建的连接往死长连接里写得进去，
	//   拿它当「出站活着」的证据，那条死长连接就永远重置不掉。
	if plan.busy["E"] {
		t.Fatal("睡眠期间新建、只写没收到过字节的连接不能证明出站还活着")
	}
}

// 只上传、暂时没有回包的连接（锁屏时的备份）也算走过字节 —— 绝不掐断正在走流量的连接。
func TestPlanWakeUploadOnlyCountsAsBusy(t *testing.T) {
	info := &statistic.TrackerInfo{Chain: C.Chain{"U"}}
	before := map[string]int64{"up": trackedBytes(info)}
	info.UploadTotal.Add(4096)
	plan := planWake(before, []wakeSample{{id: "up", bytes: trackedBytes(info), chain: info.Chain}})
	if len(plan.closeIDs) != 0 || !plan.busy["U"] {
		t.Fatalf("只上传的连接应算忙且不被关，得到 close=%v busy=%v", plan.closeIDs, plan.busy)
	}
}

// ── 执行 ──

func TestReconcileWakeClosesIdleAndResetsIdleOutbounds(t *testing.T) {
	idle := newFakeConn("idle", 100, "A", "AUTO")
	busy := newFakeConn("busy", 100, "B", "AUTO")
	before := map[string]int64{"idle": 100, "busy": 100}
	busy.info.DownloadTotal.Add(50) // 锁屏听歌：这一觉里一直有回包

	var resetA, resetB int
	proxies := map[string]C.Proxy{
		"A":    fakeProxy{adapter: shell{inner: resettable{resets: &resetA}}}, // 套着壳也要找得到
		"B":    fakeProxy{adapter: resettable{resets: &resetB}},
		"C":    fakeProxy{adapter: plainAdapter{}},
		"AUTO": fakeProxy{adapter: plainAdapter{}},
	}
	closed, reset := reconcileWake(before, []wakeTracker{idle, busy}, proxies)

	if closed != 1 || !idle.closed || busy.closed {
		t.Fatalf("只该关空闲那条：closed=%d idle=%v busy=%v", closed, idle.closed, busy.closed)
	}
	if resetA != 1 {
		t.Fatalf("A 这一觉里空闲，长连接应被重置一次，得到 %d 次", resetA)
	}
	if resetB != 0 {
		t.Fatal("B 这一觉里一直在走流量，不该重置它的长连接")
	}
	if strings.Join(reset, ",") != "A" {
		t.Fatalf("重置清单应只有 A（C/AUTO 没有长连接可重置），得到 %v", reset)
	}
}

// 真实接线：经全局连接表走一遍「挂起时快照 → 醒来时对账」。
// 睡得短（< wakeReconcileMinSleep）什么都不动；睡够了，空闲连接被关。
func TestWakeWiringThroughManager(t *testing.T) {
	c := newFakeConn("wake-wiring-idle", 42, "X")
	statistic.DefaultManager.Join(c)
	defer statistic.DefaultManager.Leave(c)

	onSleepSnapshot()
	onWakeReconcile(wakeReconcileMinSleep - time.Second)
	if c.closed {
		t.Fatal("睡得比下限短，不该动任何连接")
	}

	onSleepSnapshot()
	onWakeReconcile(10 * time.Minute)
	if !c.closed {
		t.Fatal("睡前就在、这一觉里一个字节没走过的连接应被关掉")
	}

	// 没有快照（挂起时没拍到）就不对账：没有基线，判不了谁空闲。
	c.closed = false
	onWakeReconcile(10 * time.Minute)
	if c.closed {
		t.Fatal("没有挂起时的快照，不该关任何连接")
	}
}
