// ★ darwin 约束（ios 隐含 darwin tag）：这个包只属于 iOS 线（c-archive 入口），
//   不加的话 fork 的六平台 `go test ./...` 会在非 darwin 腿上撞到
//   memfootprint_darwin.go 里那批只有 darwin 实现的符号。
//go:build darwin

// iOS 上核心必须**链进进程**，不能像 Android 那样当子进程起（没有 fork/exec）。
// 这里是给 iOS 用的 c-archive 入口：编出 libcoast.a + libcoast.h，
// 由 tunnel extension（或 app 自己，用于在模拟器上验证）直接调。
//
// ★ 只做“起/停 + 读一份配置”，其余一律走核心自己的 REST（外部控制器）——
//
//	多暴露一个函数就多一处两边要对齐的契约。
package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"fmt"
	"github.com/ClashrAuto/coast/component/suspend"
	"os"
	"path/filepath"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	Cst "github.com/ClashrAuto/coast/constant"
	"github.com/ClashrAuto/coast/hub"
	"github.com/ClashrAuto/coast/hub/executor"
	"github.com/ClashrAuto/coast/log"
	"github.com/ClashrAuto/coast/tunnel/statistic"
)

// CoastStart 起核心。
//
// homeDir  = 核心的家目录（Country.mmdb 等放这儿）
// confPath = full.yaml 的完整路径
//
// 返回 NULL 表示成功；否则是错误原文（调用方负责 free）。
//
//export CoastStart
func CoastStart(homeDir *C.char, confPath *C.char) *C.char {
	// ★ C 这层只做**字符串转换**，逻辑全在 `startCore` 里。
	//   这么分不是洁癖：**cgo 在 `_test.go` 里用不了**（`use of cgo in test not supported`），
	//   所以只要启动逻辑还长在这个带 `*C.char` 的函数里，它就**永远没法被测**——
	//   而内存探针（`memprobe_test.go`）正需要把核心真起起来。
	if err := startCore(C.GoString(homeDir), C.GoString(confPath)); err != nil {
		return C.CString(err.Error())
	}
	return nil
}

// startCore 纯 Go 的启动入口（不过 C ABI），所以测得到。
func startCore(home, conf string) error {
	applyMemoryLimit()
	captureStderr(home)

	Cst.SetHomeDir(home)
	Cst.SetConfig(conf)

	buf, err := os.ReadFile(conf)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	// ★ 与命令行那条路同一个入口（`main.go` 里也是 hub.Parse）——
	//   不另写一套启动流程，否则两边迟早漂。
	if err := hub.Parse(buf); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	// ★ 启动这一下是整个生命周期里最大的一次**瞬时**分配：解析配置、建规则树、
	//   读 Country.mmdb。这些垃圾靠 scavenger 慢慢还给系统要几分钟，而"刚连上就被杀"
	//   恰恰是最常见的时刻。所以在这里**一次性**归还。
	// ⚠️ **只这一次，不要做成定时的。** `FreeOSMemory` 会 stop-the-world 跑一轮完整 GC；
	//   周期性调它等于给每条连接的延迟加一个随机毛刺，而且持续烧 CPU（= 耗电），
	//   与下面 `applyMemoryLimit` 里"别用低 GOGC"是同一个理由。
	//   稳态下让 `SetMemoryLimit` 自己调节就够了。
	debug.FreeOSMemory()
	return nil
}

// applyMemoryLimit 给 Go 运行时装一个软内存上限。
//
// ★★★ **这是 iOS 上「核心代理会自己重启」的根因修复。**
//
//	`NEPacketTunnelProvider` 是 app extension，jetsam 对它有一个**硬**的
//	每进程内存上限：iOS 14 及以前 15 MiB，iOS 15 起 50 MiB（我们的部署目标是 26，
//	所以是 50）。超一点点就直接被杀，理由记在 JetsamEvent 里是 `per-process-limit`。
//	而 Go 默认的 GOGC=100 意味着**堆会长到存活对象的两倍**才回收 —— 大流量下
//	（成百上千条连接、gVisor 的收发缓冲、DNS 缓存）很容易越过那道线。
//
// ★★ **它的失败形态是这条链上最难查的一种**：进程是被**外部**杀掉的，
//
//	我们这侧没有 panic、没有 error、日志里最后一行是完全正常的一句。
//	系统随后按 on-demand 规则把隧道重新拉起来 —— 用户看到的就是"代理自己重启了"，
//	所有连接断掉重来，而**没有任何一条日志说明发生过什么**。
//
// ★ **为什么用 `SetMemoryLimit` 而不是调低 GOGC**：GOGC 是个恒定的比例，调低它
//
//	会让 GC **一直**更频繁地跑，哪怕内存很宽裕 —— 那是持续的 CPU 开销，
//	直接变成耗电。软上限反过来：离上限还远时按正常节奏走，逼近时才收紧。
//	Go 运行时还给它配了一道 50% 的 GC CPU 上限，所以即使设得偏小也只会**降速**，
//	不会陷入"GC 死亡螺旋"。这正是我们要的失败方向：宁可慢，不要被杀。
//
// ⚠️ **值必须明显低于 50 MiB，不能填 50。** 两个原因：
//
//	  ① 那道线管的是**整个进程**（我们的 Swift 代码、线程栈、系统框架都算），
//	     而 `SetMemoryLimit` 只管 Go 运行时自己管理的那部分 —— 二进制映射之类的
//	     外部内存不在它账上。两者不是同一个池子。
//	  ② 它是**软**上限：GC 跟不上时运行时允许短暂越界。没有余量就等于没有保护。
//	36 MiB 是留了约 14 MiB 余量的取值。
//
// ★ 可以用 `COAST_GOMEMLIMIT`（字节）覆盖，方便在真机上二分调参而不用重编库 ——
//
//	这一条**只能在设备上量**（模拟器里根本加载不了 NEPacketTunnelProvider）。
func applyMemoryLimit() {
	limit := int64(36 << 20)
	if v := os.Getenv("COAST_GOMEMLIMIT"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			limit = n
		}
	}
	debug.SetMemoryLimit(limit)
	log.Infoln("Go memory limit set to %d MiB (NE extension jetsam cap is 50 MiB)", limit>>20)
	startMemoryWatch()
}

// captureStderr 把进程的 stderr 重定向到 App Group 里的一个文件。
//
// ★★★ **它补的是一整类看不见的死法：核心 panic / Go 运行时致命错误。**
//
//	我们的日志转发（`CoastStartLogTap`）订阅的是 **mihomo 自己的 logger** ——
//	而 panic、`fatal error: concurrent map writes`、栈溢出这些是运行时直接写
//	**stderr** 的，根本不经过那个 logger。在 NE 扩展里 stderr 没有终端，
//	于是这些信息**一个字都留不下**：进程没了，日志文件最后一行完全正常。
//	⚠️ 后果是**归因错误**：这种死法和被 jetsam 杀掉长得一模一样
//	（都是"代理自己重启了、日志里什么都没有"），于是会一直往内存方向查。
//	★ 有了这个文件，两者就分得开了：
//	  · 文件里有 goroutine 栈 → 是 panic，去看那条栈；
//	  · 文件是空的、而内存日志里有 RSS 峰值 → 是 jetsam。
//
// ★ 用 `dup2` 而不是「把日志也写一份到文件」：panic 是运行时**绕过所有应用代码**
//
//	直接写 fd 2 的，只有在 fd 层面接管才拦得住。
//
// ★ 追加模式：上一次的现场比这一次的开头更值钱，别开一次覆盖一次。
//
//	⚠️ 但要有上界，否则反复 panic 会把用户的存储写满。超过 256 KiB 就从头来过。
func captureStderr(homeDir string) {
	if homeDir == "" {
		return
	}
	path := filepath.Join(homeDir, "tunnel-stderr.log")
	if st, err := os.Stat(path); err == nil && st.Size() > 256<<10 {
		_ = os.Remove(path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return // 拿不到就算了 —— 这是诊断设施，不能因为它起不来而影响代理
	}
	// ★ 不关闭 f：fd 2 从此指向它，整个进程生命周期都要活着。
	if err := syscall.Dup2(int(f.Fd()), int(os.Stderr.Fd())); err != nil {
		_ = f.Close()
		return
	}
	// ★★ 盖一行时间戳：文件是**追加**的，没有分隔的话没法知道某段栈是哪一次跑的。
	fmt.Fprintf(os.Stderr, "\n=== coast tunnel start %s ===\n",
		time.Now().Format(time.RFC3339))
}

// ── 内存看门狗 ────────────────────────────────────────────────────────────
//
// ★★★ **它存在的理由是"被 jetsam 杀掉"这件事在现场不留任何痕迹。**
//   进程是被系统从外部干掉的：没有 panic、没有 error、日志最后一行完全正常，
//   理由只记在系统自己的 JetsamEvent 里（普通用户根本看不到）。
//   于是用户只看到"代理自己重启了、连接全断",而我们这侧一无所有。
//   —— 只设上限（`applyMemoryLimit`）是把门关上，这一段是**装一个仪表**：
//   逼近上限时先喊一声，让"为什么重启"在日志页上留下证据，
//   也让 `COAST_GOMEMLIMIT` 能按真实数据二分，而不是靠推。

const (
	// NE 扩展的 jetsam 硬上限。**判据要按它算，不是按 GOMEMLIMIT 算** ——
	// 前者管**整个进程**（Swift 侧、线程栈、映射进来的二进制脏页都算），
	// 后者只管 Go 运行时那一部分。
	// ★ 2026-08-21 真机内核日志坐实了这个数：
	//   `memorystatus: CoastTunnel exceeded mem limit: ActiveHard 50 MB (fatal)`。
	// ⚠️ 但别写死信它：Apple 论坛上有 iOS 17.3.1 上超过 15 MB 就被关的报告 ——
	//   所以真机上优先用 `os_proc_available_memory` 现算（`memLimitEstimate`），
	//   这个常量只是取不到时的退路。
	neMemoryCapBytes = 50 << 20
	// 到达上限的这个比例就喊。
	memWarnAt = 0.80
	// 回落到这个比例以下才重新武装 —— **迟滞是必须的**：没有它，
	// 在阈值附近抖动会每 30 秒喊一次，把日志页（只有 100 行）刷空，
	// 那比不喊更糟（真正的诊断会被自己的告警挤掉）。
	memRearmAt = 0.65
	// ★★ **近档 2 秒，不是 30 秒。** 第一版取 30s，那是个**会漏掉现场**的取值：
	//   多线程测速能在十几秒内把内存拉满并被杀 —— 采样点很可能一次都没落在峰值附近，
	//   于是日志里什么都没有，而"没有告警"会被读成"不是内存问题"，把排查带偏。
	memPeriodNear = 2 * time.Second
	// ★★ 远档（2026-08 电池专项）：占用还不到上限六成时用 10 秒。隧道空闲时
	//   footprint 稳在 20 MB 上下一动不动，0.5 Hz 的 `proc_pidinfo` 纯属白醒。
	//   **漏现场的担心在这个档不成立**：从"六成以下"冲到被杀至少要涨 20 MB，
	//   测速那种最陡的坡也要十几秒 —— 远档在坡上至少采到一次、越过 memNearAt
	//   就切回 2 秒，此后与老行为完全一致。真正的峰值永远发生在近档里。
	memPeriodFar = 10 * time.Second
	// 占用达到上限的这个比例起用近档。
	memNearAt = 0.60
	// 峰值每涨这么多记一行 —— 不是每次采样都记（那会把日志页刷空）。
	memPeakStep = 4 << 20
)

var memWatchOnce sync.Once

// goMemoryBytes 返回 Go 运行时**实际占着**的物理内存近似值。
//
// ★ 用 `runtime/metrics` 而不是 `runtime.ReadMemStats`：后者**会 stop-the-world**，
//
//	挂在定时器上等于每 30 秒给所有连接加一个停顿 —— 而降低延迟正是这一轮要做的事。
//
// ★ 判据取 `total - released`：`total` 是**虚拟**内存（含已经还给系统的那部分），
//
//	直接用它会高估一大截；减掉 released 才接近 jetsam 真正数的那个数。
//
// ⚠️ 它只算 **Go 运行时管的**那部分。jetsam 数的是**整个进程**
//
//	（Swift 侧、线程栈、系统框架都算），所以这个值天然偏小 —— 别拿它当"离上限还有多远"
//	的精确答案，它的用途是"趋势 + 逼近时报警"。
func goMemoryBytes() uint64 {
	samples := []metrics.Sample{
		{Name: "/memory/classes/total:bytes"},
		{Name: "/memory/classes/heap/released:bytes"},
	}
	metrics.Read(samples)
	for _, s := range samples {
		if s.Value.Kind() != metrics.KindUint64 {
			return 0
		}
	}
	total, released := samples[0].Value.Uint64(), samples[1].Value.Uint64()
	if released > total {
		return total
	}
	return total - released
}

// memPeakDecision 峰值要不要记一行。**纯函数**，理由同 `memWarnDecision`。
//
// ★★★ **记峰值是这套东西里最值钱的一半。** 进程被 jetsam 杀掉时不留任何痕迹，
//
//	但隧道的日志是**落在 App Group 的文件里**的（`TunnelCoreLog`，1 Hz 刷盘）——
//	也就是说**日志能活过那次杀**。于是重启之后回头看最后几行，就知道
//	「被杀之前涨到了多少」：这是我们唯一拿得到的现场。
//	⚠️ 只记**新高**且每涨 `memPeakStep` 才记一次：每次采样都记的话，
//	  2 秒一行会把只有 100 行的日志页刷空，把真正的现场自己挤掉。
//
// lastLogged = 上次记下的那个高度。返回 (要不要记, 新的 lastLogged)。
func memPeakDecision(rss, lastLogged uint64) (bool, uint64) {
	if rss < lastLogged+memPeakStep {
		return false, lastLogged
	}
	return true, rss
}

// memWarnDecision 是**纯函数**，所以迟滞这条逻辑可以单测 ——
// 定时器那一层没法测，而真正容易写错的恰恰是这里。
//
// armed = 现在允许喊吗。返回 (要不要喊, 新的 armed)。
func memWarnDecision(used, limit uint64, armed bool) (bool, bool) {
	if limit == 0 {
		return false, armed
	}
	ratio := float64(used) / float64(limit)
	if armed && ratio >= memWarnAt {
		return true, false // 喊完就下岗，等回落
	}
	if !armed && ratio < memRearmAt {
		return false, true // 回落够了，重新武装
	}
	return false, armed
}

// processRSSBytes 整个进程的常驻内存（`statistic.DefaultManager.Memory()` →
// darwin 上走 `proc_pidinfo(PROC_PIDTASKINFO).Resident_size`）。
//
// ⚠️⚠️ **它不是 jetsam 的判据 —— 这句话上一版写反了，真机一晚就把它证伪了**：
//
//	看门狗按它报出「RSS peak 92 MiB」而进程没死；同一晚内核却在 footprint 到 50 时
//	杀了 6 次。RSS 含**干净的** text 页与 MADV_FREE 还没被收走的页，天然高估。
//	现在它只作为峰值行里的**参考读数**保留（与 footprint 的差 = 干净页 + 待回收页，
//	这个差本身有诊断价值），判据一律走 `processFootprintBytes`。
func processRSSBytes() uint64 { return statistic.DefaultManager.Memory() }

// memLimitEstimate 这一刻的 jetsam 上限。**纯函数，可单测。**
//
// ★ 真机上 `os_proc_available_memory` 给的是「还剩多少」，footprint + 剩余 = 上限 ——
//
//	系统自己算的，比写死 50 准（iOS 17.3.1 上有 15 MB 就被杀的报告）。
//	⚠️ macOS 宿主上（跑 go test 的地方）它恒 0，也可能真机上读不到 ——
//	那时退回 50 MiB 的名义值，**绝不能把 0 当上限**（那会让比例除法永远报警）。
func memLimitEstimate(footprint, available uint64) uint64 {
	if available > 0 {
		return footprint + available
	}
	return neMemoryCapBytes
}

// memPeriodDecision 下一拍的采样间隔。**纯函数，可单测。**
// 读不到内存（used/limit 为 0）时用远档：采样本身已经拿不到数据，快跑无益。
func memPeriodDecision(used, limit uint64) time.Duration {
	if limit > 0 && float64(used) >= memNearAt*float64(limit) {
		return memPeriodNear
	}
	return memPeriodFar
}

func startMemoryWatch() {
	memWatchOnce.Do(func() {
		go func() {
			armed := true
			var peakLogged uint64
			t := time.NewTimer(memPeriodNear) // 启动期按近档：起隧道正是内存最陡的一段
			for range t.C {
				// ★ 判据 = phys_footprint（jetsam 记账的那个数）。
				//   读不到（理论上不该发生）就退回 RSS —— 高估好过沉默。
				used := processFootprintBytes()
				metric := "footprint"
				if used == 0 {
					used = processRSSBytes()
					metric = "rss"
				}
				if used == 0 {
					t.Reset(memPeriodFar) // 读不到数据，快跑无益
					continue
				}
				limit := memLimitEstimate(used, processAvailableBytes())
				// ★ 先记峰值：被杀之前的最后一行就是现场。
				if newPeak, next := memPeakDecision(used, peakLogged); newPeak {
					peakLogged = next
					log.Infoln("tunnel mem peak: %s %d MiB / limit %d MiB (RSS %d, Go %d)",
						metric, used>>20, limit>>20, processRSSBytes()>>20, goMemoryBytes()>>20)
				}
				var warn bool
				warn, armed = memWarnDecision(used, limit, armed)
				if warn {
					// ★ 说清**后果**而不只是数字：用户看到"内存高"不知道该做什么，
					//   看到"再涨下去系统会把代理杀掉并自动重连"才对得上他观察到的现象。
					// ★ 同时报 Go 堆：两个数一起看才分得出「涨的是 Go 这边（缓冲/泄漏）」
					//   还是「涨在 Go 之外」，而这决定下一步该往哪查。
					log.Warnln("tunnel %s %d MiB (Go %d MiB) is near the %d MiB iOS limit; "+
						"if it keeps growing iOS will kill the tunnel and reconnect",
						metric, used>>20, goMemoryBytes()>>20, limit>>20)
				}
				t.Reset(memPeriodDecision(used, limit))
			}
		}()
	})
}

// CoastStop 停掉核心（关监听、断连接）。
//
//export CoastStop
func CoastStop() { stopCore() }

// stopCore 同 `startCore`：纯 Go，测得到。
func stopCore() {
	executor.Shutdown()
}

// CoastVersion 返回核心版本，用来在「关于」页那张卡上显示，
// 也用来确认“链进来的这一份”确实是我们以为的那一份。
//
//export CoastVersion
func CoastVersion() *C.char {
	return C.CString(Cst.Version)
}

// ── 日志转发 ──────────────────────────────────────────────────────────────
//
// ★★★ 为什么需要它：iOS 上核心**链在 app 进程里**，它写的日志谁也看不见 ——
//   而日志页的空态文案写着「核心跑起来之后它的输出会显示在这里」。
//   Android 那侧核心是子进程、stdout 被接进日志页，那句话是兑现的；
//   本线在 R114 实测过：核心真的在跑，`level=…` 一行都到不了界面。
//
// ★ **拉取式，不用回调。** 让 Swift 传一个 C 函数指针进来是可行的，但要跨语言管住
//   线程与生命周期（Go 的 goroutine 随时会调它，而 Swift 那侧对象可能已经没了）。
//   这里改成：Go 自己攒进一个**有上界**的环形缓冲，Swift 定期来取。
//   代价是延迟最多一个取用周期，换来的是边界上没有任何回调。
//
// ⚠️ **上界是产品约束，不是实现细节**：核心刷起来很快，而日志页只有 100 行。
//   缓冲满了就丢**最旧**的 —— 界面上要的是"刚才发生了什么"。

const logBufMax = 300

var (
	logMu   sync.Mutex
	logBuf  []string
	logOnce sync.Once
)

// CoastStartLogTap 开始把核心日志攒起来。幂等：重复调只订阅一次。
//
//export CoastStartLogTap
func CoastStartLogTap() {
	logOnce.Do(func() {
		sub := log.Subscribe()
		go func() {
			for e := range sub {
				// ★★★ **按配置里的 log-level 过滤，判据要和核心自己的 print() 一模一样。**
				//   `log.Subscribe()` 拿到的是**全部**级别的事件 —— 核心的级别过滤只在
				//   `print()` 里（`if data.LogLevel < level { return }`），也就是只作用于
				//   它自己的控制台输出。不在这里补一句，`log-level: info` 就形同虚设。
				//   ⚠️ 后果不是"多几行"：健康检查每轮给 56 个节点各打一条
				//   `[debug] Health Checked, proxy: …`，外加成片的
				//   `[debug] Skip once health check because we are lazy` 和 `[DNS] cache hit`。
				//   日志页只有 100 行（`CoastRepository.maxLogRows`），于是**整页恒为
				//   健康检查噪声**：核心的启动行、错误、连接行全被挤没，用户永远看不到
				//   任何可据以排查的东西 —— 而那一页的空态文案正承诺着这个。
				//   还白白跨 C ABI 每秒搬运上千条字符串。
				//   ★ 这一条是 iOS **独有**的：Android 走子进程 stdout（已被 print() 过滤），
				//     桌面两条线走 REST `/logs`（那个端点自带级别参数）——
				//     只有本线直接订阅，绕过了唯一的过滤点。
				//   ★ 级别序是 DEBUG=0 < INFO=1 < WARNING=2 < ERROR=3 < SILENT=4，
				//     所以 `<` 而不是 `>`；SILENT 作为配置值时没有任何事件能通过，正确。
				if e.LogLevel < log.Level() {
					continue
				}
				// ★★ **盖上时间戳，并用核心自己的 logfmt 形状。**
				//   `log.Subscribe()` 的事件只有级别和正文 —— 时间戳是核心的
				//   控制台格式化器加的，订阅者拿不到。不在这里盖的话，
				//   日志页那一列时间**恒为空**（2026-08-20 用户在真机上发现）。
				//   用 logfmt（`time="…" level=… msg="…"`）而不是自创格式，
				//   是因为四条线的解析器（Swift `LogLine.parse` / Kotlin `LogLine`）
				//   本来就认它 —— Android 读的核心 stdout 就是这个形状。
				//   ★ 正文里的引号要转义：解析那侧按「最后一个引号」收尾并认 \"。
				line := "time=\"" + time.Now().Format(time.RFC3339Nano) + "\" level=" +
					e.LogLevel.String() + " msg=\"" +
					strings.ReplaceAll(e.Payload, "\"", "\\\"") + "\""
				logMu.Lock()
				logBuf = append(logBuf, line)
				if len(logBuf) > logBufMax {
					// ★ 丢最旧的：界面要的是"刚才发生了什么"
					logBuf = logBuf[len(logBuf)-logBufMax:]
				}
				logMu.Unlock()
			}
		}()
	})
}

// CoastNextLog 取一条日志；没有就返回 NULL。**调用方负责 free**。
//
//export CoastNextLog
func CoastNextLog() *C.char {
	logMu.Lock()
	defer logMu.Unlock()
	if len(logBuf) == 0 {
		return nil
	}
	line := logBuf[0]
	logBuf = logBuf[1:]
	return C.CString(line)
}

func main() {}

// CoastSuspend / CoastResume —— 「设备睡了，周期性后台活动也睡」。
// 由 NEPacketTunnelProvider 的 sleep()/wake() 触发；语义与理由见核心的
// `component/suspend` 包顶（健康检查每 60 秒全节点真实握手、lazy 挡不住，
// 是「挂着 VPN 整夜耗电」的主因）。
//
//export CoastSuspend
func CoastSuspend() {
	suspend.Suspend()
}

//export CoastResume
func CoastResume() {
	suspend.Resume()
}
