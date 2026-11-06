//go:build darwin && cgo

package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ⚠️ 跑法同 libcoast_test.go：被 build_libcoast.sh 拷进内核源码树后 `go test ./mobile`。
//
// ★ 这几条在 **macOS** 上跑（GOOS=darwin），而 bug 只在 GOOS=ios 上出现 ——
//   所以每条都先把 `time.Local` 设成 UTC，**复刻 iOS 上 Go 运行时的初始状态**
//   （`zoneinfo_ios.go` 的 `initLocal` 就是 `localLoc = *UTC`），再调真身
//   `applyLocalZone("ios")`。不先设 UTC 的话，macOS 上 `time.Local` 本来就是对的，
//   断言在修复失效时照样全绿。

// hostLocal 返回 macOS 自己读 /etc/localtime 得到的时区 —— 拿它当「系统时区」的参照。
// 设了 TZ 时 Go 读的是 TZ 而 CoreFoundation 读的是系统设置，两者会不一致，跳过。
func hostLocal(t *testing.T) *time.Location {
	t.Helper()
	if os.Getenv("TZ") != "" {
		t.Skip("设了 TZ：Go 与 CoreFoundation 的参照不是同一个来源，这条判据不成立")
	}
	return time.Local
}

// 同一时刻，内核那一行 `time="…"` 里的钟点必须等于本地钟点 ——
// 也就是 Swift 侧（`ISO8601DateFormatter` + `.current`）同一秒会写出的那个钟点。
// 日志页 `LogLine.parse` 只截 `HH:mm:ss`，这里就按它的截法判。
func TestTapLineUsesLocalWallClockOnIOS(t *testing.T) {
	host := hostLocal(t)
	saved := time.Local
	defer func() { time.Local = saved }()

	time.Local = time.UTC // ← iOS 上 Go 运行时的出厂状态
	if !applyLocalZone("ios") {
		t.Fatal("applyLocalZone(\"ios\") 没换时区：CoreFoundation 取不到时区？")
	}

	now := time.Now()
	line := tapLine(now, "info", `[TUN] default interface pinned by platform: en0`)
	m := regexp.MustCompile(`time="([^"]*)"`).FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("拆不出 time=：%s", line)
	}
	stamp := m[1]
	// LogLine.hhmmss 的截法：'T' 之后 8 个字符。
	i := strings.IndexByte(stamp, 'T')
	if i < 0 || len(stamp) < i+9 {
		t.Fatalf("时间戳形状不对：%q", stamp)
	}
	shown := stamp[i+1 : i+9]
	want := now.In(host).Format("15:04:05")
	if shown != want {
		t.Fatalf("日志页会显示 %s，本地钟点是 %s（整行：%s）", shown, want, line)
	}
	// 偏移也要是本地的：截法不看偏移，但别的读者（导出、桌面）会看。
	_, off := now.In(host).Zone()
	if wantSuffix := now.In(host).Format("Z07:00"); !strings.HasSuffix(stamp, wantSuffix) {
		t.Fatalf("偏移应为 %s（%d 秒），实际 %q", wantSuffix, off, stamp)
	}
	if off == 0 {
		t.Log("⚠️ 本机时区与 UTC 同偏移：这一条在这里证伪不了修复，换一台非 UTC 时区的机器跑")
	}
}

// 换上的时区在冬夏两个时刻都与系统一致 —— 不是只对「此刻」的固定偏移。
func TestApplyLocalZoneMatchesSystemAcrossYear(t *testing.T) {
	host := hostLocal(t)
	saved := time.Local
	defer func() { time.Local = saved }()

	time.Local = time.UTC
	if !applyLocalZone("ios") {
		t.Fatal("applyLocalZone(\"ios\") 没换时区")
	}
	y := time.Now().Year()
	for _, at := range []time.Time{
		time.Now(),
		time.Date(y, time.January, 15, 12, 0, 0, 0, time.UTC),
		time.Date(y, time.July, 15, 12, 0, 0, 0, time.UTC),
	} {
		_, got := at.In(time.Local).Zone()
		_, want := at.In(host).Zone()
		if got != want {
			t.Fatalf("%s：换上的时区偏移 %d 秒，系统是 %d 秒", at.Format(time.RFC3339), got, want)
		}
	}
}

// 非 iOS 不动 `time.Local`：macOS 的 Go 自己读 /etc/localtime，本来就对。
func TestApplyLocalZoneLeavesDarwinAlone(t *testing.T) {
	saved := time.Local
	defer func() { time.Local = saved }()

	sentinel := time.FixedZone("sentinel", 12345)
	time.Local = sentinel
	if applyLocalZone("darwin") {
		t.Fatal("darwin 上不该换时区")
	}
	if time.Local != sentinel {
		t.Fatal("darwin 上 time.Local 被改了")
	}
}

// CoreFoundation 给的数据 Go 必须**解析得了**。解析不了时 zoneFrom 会静默退回
// 固定偏移 —— 钟点此刻是对的，跨夏令时就差一小时，只看结果发现不了，所以单独判。
func TestSystemZoneTZifParses(t *testing.T) {
	name, tzif, _, ok := systemZoneRaw()
	if !ok {
		t.Fatal("CoreFoundation 没给时区对象")
	}
	if name == "" {
		t.Fatal("时区名是空的")
	}
	if len(tzif) == 0 {
		t.Fatalf("%s：CFTimeZoneGetData 是空的", name)
	}
	if _, err := time.LoadLocationFromTZData(name, tzif); err != nil {
		t.Fatalf("%s：TZif 解析失败（%d 字节，头 %q）：%v", name, len(tzif), tzif[:min(len(tzif), 5)], err)
	}
}

// zoneFrom：有 TZif 就带上夏令时切换；解析不了退回固定偏移；名字缺省不留空。
func TestZoneFrom(t *testing.T) {
	const ny = "/usr/share/zoneinfo/America/New_York"
	tzif, err := os.ReadFile(ny)
	if err != nil {
		t.Skipf("读不到 %s：%v", ny, err)
	}
	loc := zoneFrom("America/New_York", tzif, 0)
	y := time.Now().Year()
	_, winter := time.Date(y, time.January, 15, 12, 0, 0, 0, time.UTC).In(loc).Zone()
	_, summer := time.Date(y, time.July, 15, 12, 0, 0, 0, time.UTC).In(loc).Zone()
	if winter != -5*3600 || summer != -4*3600 {
		t.Fatalf("纽约冬 %d / 夏 %d 秒，应为 -18000 / -14400（夏令时没带上）", winter, summer)
	}

	bad := zoneFrom("Asia/Shanghai", []byte("not a tzif"), 8*3600)
	if _, off := time.Now().In(bad).Zone(); off != 8*3600 || bad.String() != "Asia/Shanghai" {
		t.Fatalf("坏数据应退回固定偏移 +8h 且保留名字，实际 %s %d", bad, off)
	}
	if got := zoneFrom("", nil, 0).String(); got != "Local" {
		t.Fatalf("空名字应落成 Local，实际 %q", got)
	}
}
