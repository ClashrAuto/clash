//go:build darwin && cgo

// 本进程累计把 CPU 唤醒了多少次 —— 内核「caught waking the CPU … times」记的就是这本账，
// 限额是 300 秒 45000 次。
//
// ★ 取法与 phys_footprint 同一条路：`task_info` 是公开 API，自查不需要特权；
//
//	`proc_pid_rusage` 的头不在 iOS SDK 里（见 memfootprint_darwin.go）。
//
// ★ platform_idle 是 interrupt 的子集（把整个 CPU 从空闲里叫醒的那部分），不能相加。
//
//	2026-11-06 之前这里把两者加在一起，忙时多算约 15%，空闲时接近翻倍。那之前
//	PLAN 里记的唤醒数都是两者之和，要和现在的比，就把 power 行的 wakes 和 idle 加起来。
package main

/*
#include <mach/mach.h>
#include <mach/mach_time.h>

static unsigned long long coast_wakeups(void) {
	task_power_info_data_t info;
	mach_msg_type_number_t count = TASK_POWER_INFO_COUNT;
	if (task_info(mach_task_self(), TASK_POWER_INFO,
	              (task_info_t)&info, &count) != KERN_SUCCESS) {
		return 0;
	}
	return info.task_interrupt_wakeups;
}

static unsigned long long coast_idle_wakeups(void) {
	task_power_info_data_t info;
	mach_msg_type_number_t count = TASK_POWER_INFO_COUNT;
	if (task_info(mach_task_self(), TASK_POWER_INFO,
	              (task_info_t)&info, &count) != KERN_SUCCESS) {
		return 0;
	}
	return info.task_platform_idle_wakeups;
}

static unsigned long long coast_cpu_ms(void) {
	task_power_info_data_t info;
	mach_msg_type_number_t count = TASK_POWER_INFO_COUNT;
	if (task_info(mach_task_self(), TASK_POWER_INFO,
	              (task_info_t)&info, &count) != KERN_SUCCESS) {
		return 0;
	}
	// ★ 这两个字段的单位是 mach 时基，不是纳秒：arm64 上一拍 41.67 纳秒，不换算的话读数小 41 倍。
	mach_timebase_info_data_t tb;
	if (mach_timebase_info(&tb) != KERN_SUCCESS || tb.denom == 0) {
		return 0;
	}
	return (unsigned long long)((double)(info.total_user + info.total_system)
	                            * tb.numer / tb.denom / 1000000.0);
}
*/
import "C"

// taskWakeups 进程启动以来的唤醒次数。读不到时返回 0。
func taskWakeups() uint64 { return uint64(C.coast_wakeups()) }

// taskIdleWakeups 其中把 CPU 从空闲里叫醒的次数，是 taskWakeups 的子集。
func taskIdleWakeups() uint64 { return uint64(C.coast_idle_wakeups()) }

// taskCPUMillis 进程启动以来用掉的 CPU 时间（用户态 + 内核态，毫秒）。
func taskCPUMillis() uint64 { return uint64(C.coast_cpu_ms()) }

// CoastWakeups 进程累计的唤醒次数、其中的空闲唤醒次数与 CPU 毫秒。
//
//export CoastWakeups
func CoastWakeups(wakes, idleWakes, cpuMs *C.ulonglong) {
	*wakes = C.coast_wakeups()
	*idleWakes = C.coast_idle_wakeups()
	*cpuMs = C.coast_cpu_ms()
}
