//go:build darwin && cgo

package main

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ClashrAuto/coast/adapter"
	"github.com/ClashrAuto/coast/adapter/provider"
	C "github.com/ClashrAuto/coast/constant"
	P "github.com/ClashrAuto/coast/constant/provider"
	"github.com/ClashrAuto/coast/hub/executor"
	"github.com/ClashrAuto/coast/log"
	"github.com/ClashrAuto/coast/tunnel"
	"github.com/ClashrAuto/coast/tunnel/statistic"
)

// 待机：锁屏空闲时把全部节点从内核里卸掉，解锁时按当下的网络重新加载（iOS）。
//
// ★★★ 为什么是卸载，而不是继续往外加闸（2026-11-06，一夜掉 20% 之后定的）：这个内核的默认
// 行为是给常驻的机器设计的，锁屏时要它安静，得把每一种后台活动分别挡住 —— 周期测速、失败
// 体检、tide 探测、QUIC 保活、DoH 长连接…… 我们挡了三个月，每次都还漏一种，而这些活动在
// Wi-Fi 上便宜，漏了也看不出来。节点没了，这些活动就没有对象可以运行，不需要逐个知道它们。
//
// ★★ 出站一定要显式关：重载配置时旧出站只是从表里摘掉，真正的关闭靠 finalizer，要等 GC 看到
// 它们不可达，而健康检查的 goroutine 还攥着它们（要先经第一轮 GC 停掉健康检查，第二轮才轮到
// 出站），设备一冻结 GC 就不跑了。tide 的探测定时器就这样留下来过一整夜。
//
// ★ 不停内核：sing-tun 关监听时会关 utun 的 fd，而且同进程重启留下过包级状态（clash 151ac885）。
//
// ★★ 配置文件不是运行状态的全部：app 切模式只 PATCH 内核并写 config.yaml，不重建 full.yaml
// （CoastRepository.setMode，「下一次重建自然会带上它」）。以前重载只在配置变了时发生，那时
// full.yaml 是新的；现在每次解锁都重载，照文件来的话用户切到全局之后，解锁一次就悄悄变回规则。
// 所以模式在待机进出时一律沿用内核此刻的值。选中的节点不用管：它在 cache.db 里，ApplyConfig
// 自己会恢复（patchSelectGroup）。

var standby struct {
	sync.Mutex
	// 卸载时留下的测速结果，下一次加载时搬到新对象上。非 nil 说明此刻在待机。
	health map[string]adapter.Health
	// 上一次加载的节点集合。变了才让各组重新测，同一批节点解锁一次不该全量测一轮。
	nodes string
}

// enterStandby 卸掉全部节点。conf 是不带节点的配置（`TunConfig.standbyForm`）。
// 调用方要先把路由撤掉：这里会关掉全部连接，路由还在的话应用马上又连回来。
func enterStandby(conf string) error {
	cfg, err := executor.ParseWithPath(conf)
	if err != nil {
		return fmt.Errorf("parse standby config: %w", err)
	}
	standby.Lock()
	defer standby.Unlock()
	proxies, providers := tunnel.Proxies(), tunnel.Providers()
	if standby.health == nil {
		standby.health = snapshotHealth(proxies)
	}
	closed := closeConnections()
	cfg.General.Mode = tunnel.Mode()
	executor.ApplyConfig(cfg, true)
	n := closeOutbounds(proxies, providers)
	// 卸掉的节点、规则树、连接缓冲都是垃圾了，待机时内存占用低一些，系统杀我们的概率也低。
	debug.FreeOSMemory()
	// ★ Warn 级：隧道那份配置把日志压到 warning。这两行是真机上回答「待机干不干净」「解锁重新
	//   加载要多久、吃多少内存」的唯一持久痕迹，一次锁屏各一行。
	log.Warnln("[standby] unloaded %d outbound(s), closed %d connection(s), %d goroutine(s), footprint %d MiB, wakes %d",
		n, closed, runtime.NumGoroutine(), processFootprintBytes()>>20, taskWakeups())
	return nil
}

// reloadCore 重新加载配置：离开待机，以及运行中用户改了配置。conf 是完整配置。
//
// ★ 不走 REST `PUT /configs`：隧道进程自己调自己没有意义，而且这里要在 ApplyConfig 之前把测速
// 结果搬上去。不先搬的话有一段时间组里全是「没测过」，第一批连接会全落到第一个节点上。
func reloadCore(conf string) error {
	cfg, err := executor.ParseWithPath(conf)
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	standby.Lock()
	defer standby.Unlock()
	health := standby.health
	standby.health = nil
	if health == nil {
		health = snapshotHealth(tunnel.Proxies())
	} else {
		cfg.General.Mode = tunnel.Mode()
	}
	restoreHealth(cfg.Proxies, health)
	if nodes := nodeSet(cfg.Proxies); nodes != standby.nodes {
		standby.nodes = nodes
		provider.ForgetHealthChecks()
	}
	t0 := time.Now()
	executor.ApplyConfig(cfg, true)
	applied, peak := time.Since(t0), processFootprintBytes()
	debug.FreeOSMemory()
	log.Warnln("[standby] loaded %d outbound(s) in %d ms, footprint %d MiB, %d MiB after GC, %d goroutine(s), wakes %d",
		len(cfg.Proxies), applied.Milliseconds(), peak>>20, processFootprintBytes()>>20, runtime.NumGoroutine(), taskWakeups())
	return nil
}

// resetStandby 内核起来或停下时清掉上一轮的状态。proxies 是此刻加载着的节点。
func resetStandby(proxies map[string]C.Proxy) {
	standby.Lock()
	defer standby.Unlock()
	standby.health = nil
	standby.nodes = nodeSet(proxies)
}

func snapshotHealth(proxies map[string]C.Proxy) map[string]adapter.Health {
	out := make(map[string]adapter.Health, len(proxies))
	for name, p := range proxies {
		if ap, ok := p.(*adapter.Proxy); ok {
			out[name] = ap.Health()
		}
	}
	return out
}

func restoreHealth(proxies map[string]C.Proxy, health map[string]adapter.Health) {
	for name, p := range proxies {
		if ap, ok := p.(*adapter.Proxy); ok {
			if h, found := health[name]; found {
				ap.RestoreHealth(h)
			}
		}
	}
}

func closeConnections() int {
	n := 0
	statistic.DefaultManager.Range(func(c statistic.Tracker) bool {
		_ = c.Close()
		n++
		return true
	})
	return n
}

// closeOutbounds 关掉旧的提供者与出站，返回关了几个出站。先关提供者：健康检查停下来，
// 关出站时才不会有正在途中的测试把它又拨起来。
func closeOutbounds(proxies map[string]C.Proxy, providers map[string]P.ProxyProvider) int {
	seen := map[string]bool{}
	n := 0
	closeOne := func(p C.Proxy) {
		if seen[p.Name()] {
			return
		}
		seen[p.Name()] = true
		if p.Close() == nil {
			n++
		}
	}
	for _, pd := range providers {
		if c, ok := pd.(io.Closer); ok {
			_ = c.Close()
		}
	}
	for _, pd := range providers {
		for _, p := range pd.Proxies() {
			closeOne(p)
		}
	}
	for _, p := range proxies {
		closeOne(p)
	}
	return n
}

// nodeSet 节点集合的指纹。只比名字：同名节点改了地址时，测速结果按名字搬过去是旧的，但下一次
// 过期或连接失败就会更新；为这个每次都比全部字段不值得。
func nodeSet(proxies map[string]C.Proxy) string {
	names := make([]string, 0, len(proxies))
	for name := range proxies {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, "\x00")
}
