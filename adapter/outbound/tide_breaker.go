package outbound

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ClashrAuto/coast/log"
)

// dialBreaker —— TIDE 出站的连败熔断（Coast 自有，上游没有）。
//
// ★★★ 为什么需要（2026-08-26「一夜烧掉 30% 电」事故，全案在 ios/PLAN.md 当日那节）：
// 「我的电脑」这类 TIDE 对端**长时间消失是常态**（对端关机/重启/整夜不在），而协议自身的
// 恢复机制（session-grace 内 100ms→5s 重拨、redundancy 2s→30s 补路）是按「网络抖动几秒」
// 设计的。对端整夜不在时，每条新流都会造一个新会话、从头付一遍完整的拨号超时
// （SYN 打到已消失的主机没有 RST，只能等满），手机上每次推送唤醒都把射频拖起来十几秒，
// 一夜就是 30%。没有任何一层记得「这个节点已经连败几百次了」——第 500 次和第 1 次一样贵。
//
// 语义（标准三态熔断，只按**连续**失败计）：
//   - 关（openUntil 零值）：全放行。
//   - 连败 ≥ [tideBreakerThreshold] → 开：冷却期内新拨号**立即**失败（错误里带剩余秒数）。
//     快速失败正是产品行为：fallback 组（「连不上时用机场」）拿到秒级失败才切得走，
//     应用拿到立即错误才不会挂在全额超时上重试。
//   - 冷却到期 → 半开：只放**一个**探针拨号，其余仍快速失败（不然到期一瞬间的并发拨号
//     会一起打到可能还没活的对端，每个都吃满超时）。探针成功 → 关；失败 → 冷却翻倍重开
//     （[tideBreakerCooldownMin] 起步，[tideBreakerCooldownMax] 封顶）。
//   - **任何一次成功即全量复位**——恢复延迟上界 = 当前冷却长度 + 下一次拨号到来的间隔。
//
// ★ context.Canceled 不计入：那是调用方自己放弃（用户关了连接、配置重载），不是对端的账。
//   计了它，一次配置重载潮就能把好端端的节点熔断掉。DeadlineExceeded **要**计——
//   「拨到超时」正是对端消失的形态。
//
// ★ 日志纪律（手机隧道 log-level=warning、日志环 300 行）：**开**与**恢复**各一条 warning
//   （低频、是事后诊断的关键痕迹）；半开探针失败的**重开**只打 debug——对端整夜不在时
//   重开每 5 分钟一次，按 warning 打会把日志环刷穿。
//
// ★ 熔断状态在配置重载时随 adapter 重建而清零——重载本来就该给节点一次全新的机会。
//
// ★★ 「连续」按**批次**算，不按到达顺序算（2026-11-05「iOS 解锁后断网」）：
//   一次失败被记账之后，**在它之前就已放行**、此刻还在途的拨号再失败，算同一次事故，
//   不再加计数。原来按到达顺序数，于是「同一时刻并发失败 3 个」= 「连败 3 次」——
//   解锁那一刻全机 app 同时重连，撞上睡前留下的死会话（服务端早忘了它，重连被拒、
//   会话关掉、等在上面的拨号一起失败），一次事故就把熔断打开 60 秒；「不使用」档
//   直接停 1 小时、还弹一条「连不上」通知——而新会话一秒后就能建起来。
//   按批次之后，要熔断就得「失败、再拨、又失败」三轮，正是「对端真不在」的形态。
//   成功同样开新批次：成功之前放行的在途拨号后来失败了，也不算（对端刚证明过可达）。
//
// ★★ 冷却按**墙钟**算，不按单调钟（同日）：Go 的单调钟在设备睡眠时停走，手机锁屏
//   一夜醒着的时间可能只有几十分钟——「1 小时后再探一次」在墙钟上会拖成大半天，
//   用户早上解锁时「我的电脑」明明在线，熔断还开着。冷却是对人的承诺，按人的时间算。
const (
	tideBreakerThreshold   = 3
	tideBreakerCooldownMin = 60 * time.Second
	tideBreakerCooldownMax = 5 * time.Minute
	// halt 档（`halt-on-fail: true`，「我的电脑」卡片上选了「不使用」备用节点时下发）：
	// 用户的意思是「连不上就别再连了，通知我」。冷却直接给 1 小时——不是字面上的
	// 「永不再连」：每小时一次半开探针让「电脑回来后最迟一小时自动恢复」仍然成立，
	// 而一小时一拨在电池账上可以忽略。配置重载/隧道重启照样立即复位。
	tideBreakerHaltCooldown = time.Hour
)

// tideHaltEvents：halt 档的出站进入「不再连」状态时投一条（值 = 出站名），由
// 平台侧（iOS 隧道进程）取走并发系统通知。有界、非阻塞投递——没人取时宁可丢
// 事件也不能卡住拨号路径。
var tideHaltEvents = make(chan string, 8)

// TakeTideHaltEvent 取一条「已停止重试」事件；没有返回 ("", false)。
// libcoast 的 CoastNextTideHalt 轮询它。
func TakeTideHaltEvent() (string, bool) {
	select {
	case n := <-tideHaltEvents:
		return n, true
	default:
		return "", false
	}
}

// TideHaltEventsChan 把事件通道交给 REST 层流式推送（hub/route 的 /tidehalt）。
//
// ★ Android 的核心是子进程，REST 是唯一的运行时通道 —— 那侧的 app 挂一条常驻的
// 流式请求等事件（回环上的空闲 TCP，零流量零唤醒）；iOS 走上面的轮询口。
// 每个进程只有一个平台在消费，两个口共用同一条通道没有竞争问题。
func TideHaltEventsChan() <-chan string { return tideHaltEvents }

type dialBreaker struct {
	name string // 出站名，只进日志
	// halt 档：开 = 熔断后冷却 1 小时并发一次系统通知事件（见 tideBreakerHaltCooldown）。
	halt bool

	mu        sync.Mutex
	fails     int           // 连续失败数（成功清零；按批次计，见文件头）
	cooldown  time.Duration // 当前这一轮的冷却长度（半开探针失败时翻倍）
	openUntil time.Time     // 零值 = 关（墙钟，见文件头）
	probing   bool          // 半开态下已放行的那个探针还在途
	// 批次号：每记一次失败、每一次成功复位都 +1。admit 把当时的批次号发给拨号方，
	// record 时号码对不上 = 这次拨号在上一次记账之前就放行了，归入那一次事故。
	epoch uint64
	// halt 档里这一段「连不上事故」有没有通知过。成功复位时清掉——
	// 缺这个的话对端整夜不在时每小时的重开都发一条通知，比不通知更糟。
	notified bool

	now func() time.Time // 可注入，测试用
}

// breakerTicket 是 admit 发给一次被放行拨号的凭条，record 时原样交回。
type breakerTicket struct {
	epoch uint64 // 放行那一刻的批次号
	probe bool   // 它是不是半开态那唯一的探针
}

// wallNow 返回**不带单调读数**的当前时刻：之后的 Before/Sub 都按墙钟比（见文件头）。
func wallNow() time.Time { return time.Now().Round(0) }

func newDialBreaker(name string, halt bool) *dialBreaker {
	return &dialBreaker{name: name, halt: halt, now: wallNow}
}

// admit 判定这一次拨号能否放行。err == nil = 放行（凭条要在 record 时交回）；
// 返回错误 = 熔断中，调用方原样上抛。
//
// ★ 放行的每一次拨号，结果都**必须**用 record 记回来（canceled 也要记）——半开态的
//   探针名额靠 record 释放，漏记的话名额被永久占住，熔断器再也合不上。
func (b *dialBreaker) admit() (breakerTicket, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return breakerTicket{epoch: b.epoch}, nil
	}
	now := b.now()
	if now.Before(b.openUntil) {
		return breakerTicket{}, fmt.Errorf(
			"tide: circuit open after %d consecutive dial failures, retry in %ds",
			b.fails, int(b.openUntil.Sub(now)/time.Second)+1)
	}
	if b.probing {
		return breakerTicket{}, errors.New("tide: circuit half-open, probe dial already in flight")
	}
	b.probing = true
	return breakerTicket{epoch: b.epoch, probe: true}, nil
}

// record 把一次被放行的拨号结果记回来。
func (b *dialBreaker) record(t breakerTicket, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// ★ 探针名额只由**探针自己**释放。原来是「谁先回来谁释放」：半开态里一个开之前
	//   就在途的拨号先回来，会被当成探针——它的失败把熔断重开、真探针的结果反倒没人认。
	wasProbe := t.probe
	if wasProbe {
		b.probing = false
	}

	if err == nil {
		if !b.openUntil.IsZero() {
			log.Warnln("[TIDE] %s: circuit closed, dial succeeded again", b.name)
		}
		b.fails = 0
		b.cooldown = 0
		b.openUntil = time.Time{}
		b.notified = false // 这段事故结束了；下一段该重新通知
		b.epoch++          // 成功之前放行的在途拨号，后来再失败也不算
		return
	}
	if errors.Is(err, context.Canceled) {
		// 调用方自己放弃，不是对端的账；探针名额已在上面释放，下一个拨号可以再探。
		return
	}
	if !wasProbe && t.epoch != b.epoch {
		// 在上一次记账（失败或成功）之前就已放行的在途拨号：同一次事故，不再计数。
		return
	}

	b.fails++
	b.epoch++
	if wasProbe {
		// 半开探针失败：重开。halt 档冷却恒 1 小时；普通档翻倍封顶。
		// debug 而不是 warning，理由见文件头的日志纪律。
		if b.halt {
			b.cooldown = tideBreakerHaltCooldown
		} else {
			b.cooldown *= 2
			if b.cooldown > tideBreakerCooldownMax {
				b.cooldown = tideBreakerCooldownMax
			}
		}
		b.openUntil = b.now().Add(b.cooldown)
		log.Debugln("[TIDE] %s: probe failed, circuit re-opened for %ds (%d consecutive failures)",
			b.name, int(b.cooldown/time.Second), b.fails)
		return
	}
	if !b.openUntil.IsZero() {
		// 兜底：开着时只放行探针，而「开之前就已在途」的拨号已被上面的批次号挡掉，
		// 正常走不到这里。真走到了也只计数，不延长也不刷日志。
		return
	}
	if b.fails >= tideBreakerThreshold {
		if b.halt {
			b.cooldown = tideBreakerHaltCooldown
		} else {
			b.cooldown = tideBreakerCooldownMin
		}
		b.openUntil = b.now().Add(b.cooldown)
		log.Warnln("[TIDE] %s: circuit opened after %d consecutive dial failures, cooling down %ds",
			b.name, b.fails, int(b.cooldown/time.Second))
		if b.halt && !b.notified {
			b.notified = true
			// 非阻塞：队列满（没人取）就丢，绝不能卡住拨号路径。
			select {
			case tideHaltEvents <- b.name:
			default:
			}
		}
	}
}
