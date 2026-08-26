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
const (
	tideBreakerThreshold   = 3
	tideBreakerCooldownMin = 60 * time.Second
	tideBreakerCooldownMax = 5 * time.Minute
)

type dialBreaker struct {
	name string // 出站名，只进日志

	mu        sync.Mutex
	fails     int           // 连续失败数（成功清零）
	cooldown  time.Duration // 当前这一轮的冷却长度（半开探针失败时翻倍）
	openUntil time.Time     // 零值 = 关
	probing   bool          // 半开态下已放行的那个探针还在途

	now func() time.Time // 可注入，测试用
}

func newDialBreaker(name string) *dialBreaker {
	return &dialBreaker{name: name, now: time.Now}
}

// admit 判定这一次拨号能否放行。返回 nil = 放行；返回错误 = 熔断中，调用方原样上抛。
//
// ★ 放行的每一次拨号，结果都**必须**用 record 记回来（canceled 也要记）——半开态的
//   探针名额靠 record 释放，漏记的话名额被永久占住，熔断器再也合不上。
func (b *dialBreaker) admit() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return nil
	}
	now := b.now()
	if now.Before(b.openUntil) {
		return fmt.Errorf("tide: circuit open after %d consecutive dial failures, retry in %ds",
			b.fails, int(b.openUntil.Sub(now)/time.Second)+1)
	}
	if b.probing {
		return errors.New("tide: circuit half-open, probe dial already in flight")
	}
	b.probing = true
	return nil
}

// record 把一次被放行的拨号结果记回来。
func (b *dialBreaker) record(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	wasProbe := b.probing
	b.probing = false

	if err == nil {
		if !b.openUntil.IsZero() {
			log.Warnln("[TIDE] %s: circuit closed, dial succeeded again", b.name)
		}
		b.fails = 0
		b.cooldown = 0
		b.openUntil = time.Time{}
		return
	}
	if errors.Is(err, context.Canceled) {
		// 调用方自己放弃，不是对端的账；探针名额已在上面释放，下一个拨号可以再探。
		return
	}

	b.fails++
	if wasProbe {
		// 半开探针失败：冷却翻倍重开。debug 而不是 warning，理由见文件头的日志纪律。
		b.cooldown *= 2
		if b.cooldown > tideBreakerCooldownMax {
			b.cooldown = tideBreakerCooldownMax
		}
		b.openUntil = b.now().Add(b.cooldown)
		log.Debugln("[TIDE] %s: probe failed, circuit re-opened for %ds (%d consecutive failures)",
			b.name, int(b.cooldown/time.Second), b.fails)
		return
	}
	if !b.openUntil.IsZero() {
		// 开着的时候收到的是「开之前就已在途」的拨号结果：只计数，不延长也不刷日志。
		return
	}
	if b.fails >= tideBreakerThreshold {
		b.cooldown = tideBreakerCooldownMin
		b.openUntil = b.now().Add(b.cooldown)
		log.Warnln("[TIDE] %s: circuit opened after %d consecutive dial failures, cooling down %ds",
			b.name, b.fails, int(b.cooldown/time.Second))
	}
}
