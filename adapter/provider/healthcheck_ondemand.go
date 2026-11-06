package provider

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/ClashrAuto/coast/component/suspend"
)

// 按需测速（Coast 自有，两条手机线打开，桌面不变）。iOS 走 mobile/，安卓走
// 环境变量 COAST_ONDEMAND_HEALTHCHECK=1（main.go）。
//
// ★★★ 手机上没有周期测速：定时器、启动时的全量测、唤醒补测一律不跑。用到才测 ——
// 流量走到某个自动组时，这个组从没测过，或上次结果已经过期，才在后台测一轮。
// 桌面内核的默认行为是为常驻的机器设计的：测一轮要把全部节点真实握手一遍，在手机上
// 每一轮都要把射频拖起来，而屏幕关着的时候根本没有人在等这个结果。
//
// ★ 写配置关不掉：`interval: 0` 会被 outboundgroup/parser.go 改回 300，所以开关在这里。
// ★ 拨号失败触发的体检保留（组的 onDialFailed）：当前节点死了要能切走，那是修故障不是巡检。
// ★ 挂起态不测：锁屏听歌时流量一直在走，每条新连接都会摸到这里，而锁屏时没人需要换节点。
// ★ 时间用墙钟：设备睡眠时单调钟不走，拿它量「过期没有」会把一整夜量成几秒。
// ★ 「上次测过」按组名存在包里，不跟着 HealthCheck 走：iOS 锁屏时会卸载全部节点、解锁时
//   重建，新对象上的记录是空的，留在对象上的话每次解锁都要全量测一轮。

// onDemandMaxAge 测速结果的有效期。
const onDemandMaxAge = 30 * time.Minute

var (
	onDemandMode atomic.Bool

	onDemandMu       sync.Mutex
	onDemandChecked  = map[string]time.Time{}
	onDemandInFlight int
	onDemandAfter    func()

	// 单测换假钟用。
	onDemandNow = func() time.Time { return time.Now().Round(0) }
)

// SetOnDemandHealthCheck 打开或关闭按需测速。必须早于第一次加载配置。
//
// afterIdle 在所有进行中的测速都结束时调一次：测速把节点的长连接拨了起来，QUIC 与 tide 的
// 保活会一直发下去，测完要清掉没人用的。多个组常常同时开测且节点互相重叠，所以要等**全部**
// 测完 —— 只等自己这一组的话，会把别的组正在用的连接掐掉，把活节点测成死的。
func SetOnDemandHealthCheck(on bool, afterIdle func()) {
	onDemandMu.Lock()
	onDemandAfter = afterIdle
	onDemandMu.Unlock()
	onDemandMode.Store(on)
}

// ForgetHealthChecks 忘掉各组「上次测过」的记录，下一次使用时重新测。节点集合变了时用：
// 新节点没有测速结果，等上半小时才测不合理。
func ForgetHealthChecks() {
	onDemandMu.Lock()
	defer onDemandMu.Unlock()
	onDemandChecked = map[string]time.Time{}
}

// claimOnDemandCheck 这个组现在该不该开测。一波连接里每条拨号都会摸到这里，只有第一条拿到。
func claimOnDemandCheck(name string) bool {
	onDemandMu.Lock()
	defer onDemandMu.Unlock()
	now := onDemandNow()
	if last, ok := onDemandChecked[name]; ok {
		// 差为负说明系统时间被往回调过，那条记录不可信，按过期算。
		if age := now.Sub(last); age >= 0 && age < onDemandMaxAge {
			return false
		}
	}
	onDemandChecked[name] = now
	onDemandInFlight++
	return true
}

func beginOnDemandCheck() {
	onDemandMu.Lock()
	onDemandInFlight++
	onDemandMu.Unlock()
}

func finishOnDemandCheck(name string) {
	onDemandMu.Lock()
	onDemandChecked[name] = onDemandNow()
	onDemandInFlight--
	var after func()
	if onDemandInFlight == 0 {
		after = onDemandAfter
	}
	onDemandMu.Unlock()
	if after != nil {
		after()
	}
}

// touchOnDemand 见文件头。每次经组拨号都会走到，只做一次锁内的查表。
func (bp *baseProvider) touchOnDemand() {
	if !bp.healthCheck.auto() || suspend.Suspended() || !claimOnDemandCheck(bp.name) {
		return
	}
	go func() {
		defer finishOnDemandCheck(bp.name)
		bp.healthCheck.check()
	}()
}
