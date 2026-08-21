//go:build darwin && cgo

// phys_footprint —— jetsam 真正记账的那个数。
//
// ★★★ **RSS 不是 jetsam 的判据，这一课是真机上花了一晚学的**（2026-08-21）：
//
//	看门狗按 RSS 报出「tunnel RSS peak 92 MiB」而进程活得好好的，
//	同一晚内核却以「exceeded mem limit: ActiveHard 50 MB (fatal)」杀了它 6 次。
//	两件事都是真的：jetsam 数的是 **phys_footprint**（脏页 + 压缩内存），
//	而 RSS 还包含**干净的**文件映射页（52 MB 大二进制被执行到的 text 段）和
//	Go 用 MADV_FREE 还给系统、内核还没来得及收走的页 —— 这些都不算 footprint。
//	于是 RSS 天然**高估**，最坏时高出近一倍：拿它当仪表，读数逼近 50 时
//	既可能真要死了、也可能还有一半余量 —— 等于没有仪表。
//
// ★ 取法是 `task_info(mach_task_self(), TASK_VM_INFO).phys_footprint` ——
//
//	Apple 自家《iOS Memory Deep Dive》里点名的那本账，公开 API，自查不需要特权。
//	⚠️ **不能用 `proc_pid_rusage`**：它声明在 `libproc.h` 里，而那个头
//	**不在 iOS SDK 里**（macOS 专属）—— 第一版就是这么写的，设备切片当场编不过。
//
// ★ `os_proc_available_memory()`（iOS 13+）：**到 jetsam 上限还剩多少**，
//
//	系统自己算的，不用我们假设上限是 50 —— iOS 17.3.1 上有 15 MB 就被杀的报告，
//	写死 50 在那种设备上会晚报警。⚠️ 它在 macOS 上（跑 `go test` 的宿主）恒 0，
//	所以取不到时要退回名义上限，见 `memLimitEstimate`。
package main

/*
#include <mach/mach.h>
#include <TargetConditionals.h>
#if TARGET_OS_IPHONE
#include <os/proc.h>
#endif

static unsigned long long coast_phys_footprint(void) {
	task_vm_info_data_t info;
	mach_msg_type_number_t count = TASK_VM_INFO_COUNT;
	if (task_info(mach_task_self(), TASK_VM_INFO,
	              (task_info_t)&info, &count) != KERN_SUCCESS) {
		return 0;
	}
	// phys_footprint 是 REV1 才有的字段；老内核给的 count 不够长就别读（读了是垃圾）。
	if (count < TASK_VM_INFO_REV1_COUNT) {
		return 0;
	}
	return (unsigned long long)info.phys_footprint;
}

static unsigned long long coast_available_memory(void) {
// ★ `os_proc_available_memory` 在 macOS 的 SDK 里被标成 unavailable，**编译期**就拒 ——
//   不用 TARGET_OS_IPHONE 括起来的话，`go test`（macOS 宿主）根本编不过。
//   macOS 上返回 0 = 「读不到」，`memLimitEstimate` 会退回名义上限。
#if TARGET_OS_IPHONE
	return (unsigned long long)os_proc_available_memory();
#else
	return 0;
#endif
}
*/
import "C"

// processFootprintBytes 整个进程的 phys_footprint（字节）。读不到返回 0。
func processFootprintBytes() uint64 { return uint64(C.coast_phys_footprint()) }

// processAvailableBytes 距离 jetsam 上限还剩的字节数。
// macOS 宿主（跑测试的地方）上恒 0；真机上 0 也可能表示"读不到"，
// 所以它只用来**修正**上限估计，绝不单独当判据。
func processAvailableBytes() uint64 { return uint64(C.coast_available_memory()) }
