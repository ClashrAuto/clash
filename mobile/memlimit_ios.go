//go:build ios && cgo

// iOS 的内存参数。**这是「核心代理会自己重启」的唯一防线** ——
// 完整的理由（jetsam 为什么杀、为什么用 SetMemoryLimit 而不是 GOGC、
// 为什么留 14 MiB 余量）写在 libcoast.go 的 `applyMemoryLimit` 上方，不在这里复述。
//
// ⚠️ **改这里的数之前先读那段。** 36 这个值是真机上量出来的
// （commit 1a420f56），不是拍脑袋定的；调大一点点就会重新开始被 jetsam 杀，
// 而失败形态是「连接全断了一次」且**日志里没有任何一行说明发生过什么**。
package main

const (
	// iOS 15 起 jetsam 的每进程上限是 50 MiB，36 留了约 14 MiB 余量。
	defaultMemLimit int64 = 36 << 20
	// 装仪表：真机上逼近上限时要有读数，否则被杀时手上什么都没有。
	memWatchEnabled = true
	memLimitReason  = "NE extension jetsam cap is 50 MiB"
)
