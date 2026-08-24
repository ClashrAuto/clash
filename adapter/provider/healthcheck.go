package provider

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/ClashrAuto/coast/common/atomic"
	"github.com/ClashrAuto/coast/common/singledo"
	"github.com/ClashrAuto/coast/common/utils"
	"github.com/ClashrAuto/coast/component/suspend"
	C "github.com/ClashrAuto/coast/constant"
	"github.com/ClashrAuto/coast/log"

	"github.com/dlclark/regexp2"
	"golang.org/x/sync/errgroup"
)

type HealthCheckOption struct {
	URL      string
	Interval uint
}

// ── Coast：跨 HealthCheck 实例的探测闸门 ─────────────────────────────────────
//
// 同一个节点常被多个组引用（种子把每个订阅节点放进 AUTO + 订阅组 + 地区组，恰好 3 个），
// 而**每个组各持一套独立的 HealthCheck**（outboundgroup/parser.go 里每组 NewHealthCheck），
// 下面的 singleDo 只在单个实例内去重。于是所有「全量开测」的时刻——加载/热重载
// （process() 起手无条件 check()）、唤醒补测、以及 REST 测组摸了 lastTouch 之后的下一个
// 周期 tick——同一个节点会被并发真实握手 3 次。实测一份 57 节点的配置每次重载打出
// 171 个探测请求。
//
// 这里按「节点名 + 测速 URL」共享一个 singledo.Single：并发的重复调用合流成一次，
// 刚测完的（窗口内）直接复用结果。窗口取 10s：
//   · 足够盖住「多组同时开测」（它们的间隔是毫秒级）；
//   · 远小于常用 interval（种子写 60s），不会吞掉正常的周期轮，
//     用户自定义的激进 interval（≥15s 都安全）也不受影响。
// ⚠️ 手动测速（REST /proxies/<name>/delay）直接调 Proxy.URLTest，**不经这里** ——
//    用户点一下就该真测一次，闸门只拦后台健康检查自己的重复。
// ⚠️ key 不含 expectedStatus：同 URL 不同 expected-status 的两个组会共享结果，
//    记录侧（URLTest 的 defer）本就按 URL 归档，语义一致。
// ⚠️ 复用/合流拿到的是**发起方**那次调用的结果；发起方的 HC 在热重载中被关闭时，
//    等待方会分到一次 ctx 取消的失败样本——下一轮（≥interval）就自愈，不值得为它加锁序。
type probeGate struct {
	mu      sync.Mutex
	entries map[string]*probeGateEntry
}

type probeGateEntry struct {
	single   *singledo.Single[struct{}]
	lastUsed time.Time
}

const probeReuseWindow = 10 * time.Second

var sharedProbeGate = &probeGate{entries: map[string]*probeGateEntry{}}

// forKey 取（必要时建）这个 key 的合流器，并顺手把闲置太久的条目清掉——
// 节点随订阅增删，key 集合会漂，不清就是跨重载的慢泄漏。
func (g *probeGate) forKey(key string) *singledo.Single[struct{}] {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.entries[key]
	if !ok {
		if len(g.entries) > 1024 {
			cutoff := time.Now().Add(-5 * time.Minute)
			for k, v := range g.entries {
				if v.lastUsed.Before(cutoff) {
					delete(g.entries, k)
				}
			}
		}
		e = &probeGateEntry{single: singledo.NewSingle[struct{}](probeReuseWindow)}
		g.entries[key] = e
	}
	e.lastUsed = time.Now()
	return e.single
}

type extraOption struct {
	expectedStatus utils.IntRanges[uint16]
	filters        map[string]struct{}
}

type HealthCheck struct {
	ctx            context.Context
	ctxCancel      context.CancelFunc
	url            string
	extra          map[string]*extraOption
	mu             sync.Mutex
	proxies        []C.Proxy
	interval       time.Duration
	lazy           bool
	expectedStatus utils.IntRanges[uint16]
	lastTouch      atomic.TypedValue[time.Time]
	singleDo       *singledo.Single[struct{}]
	timeout        time.Duration
}

func (hc *HealthCheck) process() {
	ticker := time.NewTicker(hc.interval)
	go hc.check()
	// ★ Coast：设备醒来时补一轮 —— 只补「挂起前最近被用过」的组（与 lazy 同一判据），
	//   没人用的组醒来也不用测。key 用 hc 自身指针，close 时成对注销（热重载会走这对）。
	suspend.RegisterResumeHook(hc, func() {
		if !hc.lazy || time.Since(hc.lastTouch.Load()) < hc.interval {
			go hc.check()
		}
	})
	for {
		select {
		case <-ticker.C:
			// ★ Coast：挂起态（设备睡眠/息屏）跳过周期探测 —— 这正是本产品
			//   「挂着 VPN 整夜耗电」的主因（每 60 秒全节点真实握手，lazy 挡不住：
			//   lastTouch 在每次经组拨号时都被摸，而手机永远有后台滴流）。
			//   隧道转发不受影响，停的只是我们主动发起的探测；恢复逻辑见上面的 hook。
			if suspend.Suspended() {
				continue
			}
			lastTouch := hc.lastTouch.Load()
			since := time.Since(lastTouch)
			if !hc.lazy || since < hc.interval {
				hc.check()
			} else {
				log.Debugln("Skip once health check because we are lazy")
			}
		case <-hc.ctx.Done():
			ticker.Stop()
			suspend.UnregisterResumeHook(hc)
			return
		}
	}
}

func (hc *HealthCheck) setProxies(proxies []C.Proxy) {
	hc.proxies = proxies
}

func (hc *HealthCheck) registerHealthCheckTask(url string, expectedStatus utils.IntRanges[uint16], filter string, interval uint) {
	url = strings.TrimSpace(url)
	if len(url) == 0 || url == hc.url {
		log.Debugln("ignore invalid health check url: %s", url)
		return
	}

	hc.mu.Lock()
	defer hc.mu.Unlock()

	// if the provider has not set up health checks, then modify it to be the same as the group's interval
	if hc.interval == 0 {
		hc.interval = time.Duration(interval) * time.Second
	}

	if hc.extra == nil {
		hc.extra = make(map[string]*extraOption)
	}

	// prioritize the use of previously registered configurations, especially those from provider
	if _, ok := hc.extra[url]; ok {
		// provider default health check does not set filter
		if url != hc.url && len(filter) != 0 {
			splitAndAddFiltersToExtra(filter, hc.extra[url])
		}

		log.Debugln("health check url: %s exists", url)
		return
	}

	option := &extraOption{filters: map[string]struct{}{}, expectedStatus: expectedStatus}
	splitAndAddFiltersToExtra(filter, option)
	hc.extra[url] = option
}

func splitAndAddFiltersToExtra(filter string, option *extraOption) {
	filter = strings.TrimSpace(filter)
	if len(filter) != 0 {
		for _, regex := range strings.Split(filter, "`") {
			regex = strings.TrimSpace(regex)
			if len(regex) != 0 {
				option.filters[regex] = struct{}{}
			}
		}
	}
}

func (hc *HealthCheck) auto() bool {
	return hc.interval != 0
}

func (hc *HealthCheck) touch() {
	hc.lastTouch.Store(time.Now())
}

func (hc *HealthCheck) check() {
	if len(hc.proxies) == 0 {
		return
	}

	_, _, _ = hc.singleDo.Do(func() (struct{}, error) {
		id := utils.NewUUIDV4().String()
		log.Debugln("Start New Health Checking {%s}", id)
		b := new(errgroup.Group)
		b.SetLimit(10)

		// execute default health check
		option := &extraOption{filters: nil, expectedStatus: hc.expectedStatus}
		hc.execute(b, hc.url, id, option)

		// execute extra health check
		if len(hc.extra) != 0 {
			for url, option := range hc.extra {
				hc.execute(b, url, id, option)
			}
		}
		_ = b.Wait()
		log.Debugln("Finish A Health Checking {%s}", id)
		return struct{}{}, nil
	})
}

func (hc *HealthCheck) execute(b *errgroup.Group, url, uid string, option *extraOption) {
	url = strings.TrimSpace(url)
	if len(url) == 0 {
		log.Debugln("Health Check has been skipped due to testUrl is empty, {%s}", uid)
		return
	}

	var filterReg *regexp2.Regexp
	var expectedStatus utils.IntRanges[uint16]
	if option != nil {
		expectedStatus = option.expectedStatus
		if len(option.filters) != 0 {
			filters := make([]string, 0, len(option.filters))
			for filter := range option.filters {
				filters = append(filters, filter)
			}

			filterReg = regexp2.MustCompile(strings.Join(filters, "|"), regexp2.None)
		}
	}

	for _, proxy := range hc.proxies {
		// skip proxies that do not require health check
		if filterReg != nil {
			if match, _ := filterReg.MatchString(proxy.Name()); !match {
				continue
			}
		}

		p := proxy
		b.Go(func() error {
			// Coast：经共享闸门测——并发的重复请求（别的组的同一节点）合流成一次，
			// 窗口内刚测过的直接复用。见文件顶部 probeGate 的说明。
			_, _, shared := sharedProbeGate.forKey(p.Name() + "|" + url).Do(func() (struct{}, error) {
				ctx, cancel := context.WithTimeout(hc.ctx, hc.timeout)
				defer cancel()
				log.Debugln("Health Checking, proxy: %s, url: %s, id: {%s}", p.Name(), url, uid)
				_, _ = p.URLTest(ctx, url, expectedStatus)
				log.Debugln("Health Checked, proxy: %s, url: %s, alive: %t, delay: %d ms uid: {%s}", p.Name(), url, p.AliveForTestUrl(url), p.LastDelayForTestUrl(url), uid)
				return struct{}{}, nil
			})
			if shared {
				log.Debugln("Health Check reused, proxy: %s, url: %s, id: {%s}", p.Name(), url, uid)
			}
			return nil
		})
	}
}

func (hc *HealthCheck) close() {
	hc.ctxCancel()
}

func NewHealthCheck(proxies []C.Proxy, url string, timeout uint, interval uint, lazy bool, expectedStatus utils.IntRanges[uint16]) *HealthCheck {
	if url == "" {
		expectedStatus = nil
		interval = 0
	}
	if timeout == 0 {
		timeout = 5000
	}
	ctx, cancel := context.WithCancel(context.Background())

	return &HealthCheck{
		ctx:            ctx,
		ctxCancel:      cancel,
		proxies:        proxies,
		url:            url,
		timeout:        time.Duration(timeout) * time.Millisecond,
		extra:          map[string]*extraOption{},
		interval:       time.Duration(interval) * time.Second,
		lazy:           lazy,
		expectedStatus: expectedStatus,
		singleDo:       singledo.NewSingle[struct{}](time.Second),
	}
}
