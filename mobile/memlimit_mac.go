//go:build darwin && !ios && cgo

// macOS 的内存参数。
//
// ★★★ **约束是 `darwin && !ios`，那个 `!ios` 不能省。**
//
//	Go 里 iOS 的 GOOS 是 `ios`，而它**隐含 darwin tag** —— 只写 `darwin` 的话
//	这个文件在 iOS 上也会参与编译，与 memlimit_ios.go 撞成「常量重复声明」。
//	更坏的情形是反过来：如果哪天有人把 iOS 那份的约束放宽，iOS 就会拿到下面这个
//	512 MiB 的缺省值 —— 那等于把 commit 1a420f56 修好的
//	「代理自己重启」原样退回去，且照旧零报错。
//
// ★★ **macOS 的 System Extension 没有 jetsam 的每进程硬上限。**
//
//	iOS 那个 36 MiB 是**防被杀**的防线；macOS 这里没有那道门，所以取值逻辑
//	完全不同 —— 这里的 512 MiB 是**防失控**的护栏，两者不是同一件事，
//	别拿一边的理由去论证另一边的数。
//
// ⚠️ **为什么不干脆不设上限**：Go 默认 GOGC=100，堆会长到存活对象的两倍才回收。
//
//	代理核心在大流量下（成百上千条连接、gVisor 收发缓冲、DNS 缓存）没有上界时
//	可以涨得很难看。512 MiB 对这个负载**极其宽裕**（同一份核心在 iOS 上 36 MiB
//	就能跑完整代理），所以正常使用中根本够不着它，GC 不会因为它而变频繁 ——
//	它只在真的失控时才收紧。
//
// ★ 仪表关掉：`startMemoryWatch` 是为了在 iOS 上盯着 footprint 逼近 50 那道线
//
//	而装的。macOS 上没有那道线要盯，2 秒一次的采样就是纯开销。
//	真要在 macOS 上排查内存，用 Instruments 或核心自己的 /memory，
//	比这个 2 秒采样的看门狗准得多。
//
// ★ `COAST_GOMEMLIMIT` 在两个平台上一样管用（解析在 libcoast.go 的
//
//	`resolveMemLimit` 里，平台无关）。
package main

const (
	defaultMemLimit int64 = 512 << 20
	memWatchEnabled       = false
	memLimitReason        = "macOS sysex has no jetsam per-process cap; this is a runaway guard"
)
