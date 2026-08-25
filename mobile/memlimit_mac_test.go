//go:build darwin && !ios && cgo

package main

import "testing"

// macOS 的平台常量。
//
// ★★ **这条门禁挡的是「照抄 iOS」。** 把 36 MiB 抄到 macOS 上不会报错、
//
//	不会重启、不会有任何一条日志 —— 只会让 Go 的 GC 疯转、吞吐大幅下降。
//	「慢」是这条链上最难归因的失败形态：查的人会去看网络、看节点、看核心，
//	唯独不会想到是内存上限。所以把它钉死在测试里。
func TestMacMemDefaults(t *testing.T) {
	if defaultMemLimit != 512<<20 {
		t.Fatalf("macOS 缺省上限应当是 512 MiB（防失控的护栏，不是防杀的防线），实际 %d", defaultMemLimit)
	}
	// ★ 上面那条是精确值，这条是**方向**：即使将来有理由调整 512，
	//   也绝不该落回 iOS 那个量级 —— 那说明有人把两边的理由弄混了。
	if defaultMemLimit <= 36<<20 {
		t.Fatal("macOS 的上限不该收到 iOS 那个量级 —— System Extension 没有 jetsam 每进程硬上限")
	}
	if memWatchEnabled {
		t.Fatal("macOS 上不该起内存看门狗：没有 50 MiB 那道线要盯，2 秒一次的采样是纯开销")
	}
}
