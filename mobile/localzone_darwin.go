//go:build darwin && cgo

// 给 iOS 上的 Go 运行时补上「本地时区」。
//
// ★★★ **Go 在 iOS 上根本没实现本地时区 —— `time.Local` 恒为 UTC。**
//
//	`$GOROOT/src/time/zoneinfo_ios.go` 的 `initLocal()` 原文就是一句
//	`localLoc = *UTC`（旁边挂着 `TODO(crawshaw): [NSTimeZone localTimeZone]`）：
//	不读 `TZ`、不读 `/etc/localtime`、也没有任何 zoneinfo 来源
//	（`platformZoneSources` 在 iOS 上是空的）。所以这**不是**沙箱读不到文件，
//	设环境变量也救不回来。macOS（GOOS=darwin）走的是 `zoneinfo_unix.go`，
//	读 `/etc/localtime`，本来就对 —— 这一处只对 iOS 生效。
//
// ★ 后果（2026-11-05 真机）：日志页上内核那一半的行（`[TUN] …`、`[TIDE] …`）
//
//	比同一秒 Swift 侧写的行（`TunnelCoreLog.note`，`ISO8601DateFormatter`
//	+ `timeZone = .current`）**早 8 小时**：Swift 17:07:52、内核 09:07:52，
//	列表看着乱序。日志页的 `LogLine.parse` 只截 `time="…"` 里的 `HH:mm:ss`
//	（四条线同一套，Android 读的核心 stdout 本来就是本地时间），所以内核一写 `Z`
//	界面上就是 UTC 的钟点 —— 修在写的这一侧，解析器不动。
//
// ★★ **从 CoreFoundation 取时区的 TZif 原始数据，而不是只取「此刻偏移」。**
//
//	`time.FixedZone(name, 此刻偏移)` 更省事，但扩展进程一跑就是好几天，
//	跨过夏令时切换后就会差整整一小时，而且零报错。`CFTimeZoneGetData` 给的
//	就是系统自己那份 zoneinfo 文件，交给 `time.LoadLocationFromTZData`
//	拿到的是带全部切换点的完整时区 —— 不用为它嵌 `time/tzdata`
//	（约 450 KB，扩展有 50 MiB 的 jetsam 上限，能不加就不加）。
//	TZif 解析不了才退回固定偏移：钟点在下一次切换前仍然是对的，比退回 UTC 强。
//
// ★ 不走 C 接口让 Swift 传进来：那要多一个两边对齐的导出函数（见 libcoast.go 文件头
//
//	「多暴露一个函数就多一处契约」），而 CoreFoundation 在这个进程里本来就在。
//	取的是 `CFTimeZoneCopyDefault()` —— 与 Swift 侧 `TimeZone.current` 同一个来源，
//	两边的钟点由此对齐（扩展里没人改 default，它就等于系统时区）。
//	⚠️ 两边都是**进程启动时**取一次：用户中途换了时区，内核行与 Swift 行
//	一起停在旧时区，彼此仍然一致（Swift 那个格式化器也是取一次就缓存的）。
package main

/*
#cgo LDFLAGS: -framework CoreFoundation
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>

// 取进程默认时区：名字（UTF-8）、TZif 原始数据、此刻相对 GMT 的秒数。
// name / data 由调用方 free；拿不到的项留 NULL / 0。返回 0 = 连时区对象都没有。
static int coastSystemZone(char **name, unsigned char **data, long *dataLen, int *offset) {
	*name = NULL; *data = NULL; *dataLen = 0; *offset = 0;
	CFTimeZoneRef tz = CFTimeZoneCopyDefault();
	if (tz == NULL) return 0;
	*offset = (int)CFTimeZoneGetSecondsFromGMT(tz, CFAbsoluteTimeGetCurrent());
	CFStringRef n = CFTimeZoneGetName(tz);   // Get 规则：不归我们释放
	if (n != NULL) {
		CFIndex max = CFStringGetMaximumSizeForEncoding(CFStringGetLength(n), kCFStringEncodingUTF8) + 1;
		char *buf = malloc((size_t)max);
		if (buf != NULL && CFStringGetCString(n, buf, max, kCFStringEncodingUTF8)) {
			*name = buf;
		} else {
			free(buf);
		}
	}
	CFDataRef d = CFTimeZoneGetData(tz);     // Get 规则：不归我们释放
	if (d != NULL) {
		CFIndex len = CFDataGetLength(d);
		if (len > 0) {
			unsigned char *buf = malloc((size_t)len);
			if (buf != NULL) {
				CFDataGetBytes(d, CFRangeMake(0, len), buf);
				*data = buf;
				*dataLen = (long)len;
			}
		}
	}
	CFRelease(tz);
	return 1;
}
*/
import "C"

import (
	"runtime"
	"time"
	"unsafe"
)

// ★ 放在 init 里：c-archive 的包初始化在任何导出函数返回之前就跑完，
//
//	于是日志 tap、captureStderr 的分隔行、内核自己格式化的时间
//	从第一行起就是本地时间，不用在每个入口各补一次（漏一个就又是 UTC）。
func init() { applyLocalZone(runtime.GOOS) }

// applyLocalZone 只在 iOS 上把 `time.Local` 换成系统时区；返回是否换了。
// 参数是 GOOS 而不是直接读 runtime.GOOS，为的是 macOS 上的测试能走到 iOS 那条分支
// （测的是真身，不是手写的等价物）。
func applyLocalZone(goos string) bool {
	if goos != "ios" {
		return false // darwin 的 initLocal 读 /etc/localtime，本来就对，别动它
	}
	loc := systemZone()
	if loc == nil {
		return false // 连时区对象都拿不到：保持 UTC（带 Z，至少不会被读成本地时间）
	}
	time.Local = loc
	return true
}

// systemZone 从 CoreFoundation 取当前时区；拿不到返回 nil。
func systemZone() *time.Location {
	name, tzif, off, ok := systemZoneRaw()
	if !ok {
		return nil
	}
	return zoneFrom(name, tzif, off)
}

// systemZoneRaw 是 CoreFoundation 给的原料（名字、TZif 原始数据、此刻偏移秒数）。
// 单拎出来是为了让测试能直接判「这份数据 Go 解析得了」——
// 解析不了时 zoneFrom 会静默退回固定偏移，只看结果的话这件事是看不出来的。
func systemZoneRaw() (name string, tzif []byte, offsetSec int, ok bool) {
	var cname *C.char
	var data *C.uchar
	var n C.long
	var off C.int
	if C.coastSystemZone(&cname, &data, &n, &off) == 0 {
		return "", nil, 0, false
	}
	defer C.free(unsafe.Pointer(cname))
	defer C.free(unsafe.Pointer(data))
	if cname != nil {
		name = C.GoString(cname)
	}
	if data != nil && n > 0 {
		tzif = C.GoBytes(unsafe.Pointer(data), C.int(n))
	}
	return name, tzif, int(off), true
}

// zoneFrom TZif 解析得了就用它（带全部夏令时切换点），否则退回此刻偏移的固定时区。
func zoneFrom(name string, tzif []byte, offsetSec int) *time.Location {
	if name == "" {
		name = "Local"
	}
	if len(tzif) > 0 {
		if loc, err := time.LoadLocationFromTZData(name, tzif); err == nil {
			return loc
		}
	}
	return time.FixedZone(name, offsetSec)
}
