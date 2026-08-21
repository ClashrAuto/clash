//go:build darwin && cgo

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"testing"
	"time"
)

// 内存上限**真的装上了**，而且能被环境变量覆盖。
//
// ★★ 这一条值得单独测：`applyMemoryLimit` 是 iOS 上「核心被 jetsam 杀掉、
//
//	隧道自己重启」的唯一防线，而它**失效时毫无症状** —— 少调一次、值填错一个数量级，
//	在开发机和模拟器上都完全看不出来（那两处根本没有 NE 的内存上限）。
//	只有真机在大流量下才会现形，而现形的方式是"连接全断了一次"。
//
// ⚠️ 跑法：这个文件与 `libcoast.go` 一起被 `build_libcoast.sh` 拷进内核源码树，
//
//	在那里 `go test ./mobile` —— 本仓库没有 Go 的模块上下文，直接在这里跑不了。
func TestApplyMemoryLimit(t *testing.T) {
	// SetMemoryLimit(-1) 只读不写，用来看当前值。
	old := debug.SetMemoryLimit(-1)
	defer debug.SetMemoryLimit(old)

	applyMemoryLimit()
	if got := debug.SetMemoryLimit(-1); got != 36<<20 {
		t.Fatalf("缺省上限应当是 36 MiB，实际 %d", got)
	}

	// ★ 覆盖这条路是给真机上二分调参用的 —— 它坏掉的话人会以为"调了没用"，
	//   然后把结论归到内存上限这个方案本身不管用。
	t.Setenv("COAST_GOMEMLIMIT", "20971520") // 20 MiB
	applyMemoryLimit()
	if got := debug.SetMemoryLimit(-1); got != 20<<20 {
		t.Fatalf("环境变量没生效，实际 %d", got)
	}

	// ★ 垃圾值必须被忽略而不是把上限设成 0 —— 设成 0 等于"每次分配都 GC"，
	//   那比没有上限还糟（CPU 烧满、吞吐归零），而且同样零报错。
	t.Setenv("COAST_GOMEMLIMIT", "abc")
	applyMemoryLimit()
	if got := debug.SetMemoryLimit(-1); got != 36<<20 {
		t.Fatalf("垃圾值应当退回缺省，实际 %d", got)
	}
	t.Setenv("COAST_GOMEMLIMIT", "0")
	applyMemoryLimit()
	if got := debug.SetMemoryLimit(-1); got != 36<<20 {
		t.Fatalf("0 应当退回缺省，实际 %d", got)
	}
}

// 内存看门狗的**迟滞**逻辑。
//
// ★★ 抽成纯函数就是为了能测这一条：定时器那层测不了，而真正容易写错的恰恰是迟滞 ——
//
//	没有它，在阈值附近抖动会每 30 秒喊一次，把只有 100 行的日志页刷空，
//	**比不喊更糟**（真正的诊断被自己的告警挤掉）。
func TestMemWarnDecision(t *testing.T) {
	const limit = 100

	// 低于阈值：不喊，保持武装
	if warn, armed := memWarnDecision(50, limit, true); warn || !armed {
		t.Fatalf("50%% 不该喊，得到 warn=%v armed=%v", warn, armed)
	}
	// 到 80%：喊一次，然后下岗
	warn, armed := memWarnDecision(80, limit, true)
	if !warn || armed {
		t.Fatalf("80%% 该喊且下岗，得到 warn=%v armed=%v", warn, armed)
	}
	// 还在高位：**不再重复喊**（这就是迟滞要挡的）
	if warn, _ := memWarnDecision(95, limit, false); warn {
		t.Fatal("下岗之后不该重复喊")
	}
	// 回落到 70%：仍在 65% 之上，**还不重新武装** —— 否则 65~80 之间抖动照样刷屏
	if _, armed := memWarnDecision(70, limit, false); armed {
		t.Fatal("70%% 还不该重新武装（重整线是 65%%）")
	}
	// 回落到 60%：重新武装
	if _, armed := memWarnDecision(60, limit, false); !armed {
		t.Fatal("60%% 该重新武装")
	}
	// ★ limit 为 0 时不许除零、也不许喊
	if warn, _ := memWarnDecision(10, 0, true); warn {
		t.Fatal("limit=0 时不该喊")
	}
}

// 判据必须按 **jetsam 的上限**算，不是按 GOMEMLIMIT 算。
//
// ★★ 这一条钉的是我改对的那件事：RSS 是**整个进程**的，而 GOMEMLIMIT 只管 Go 那部分。
//
//	拿 36 MiB 当分母的话，进程 40 MiB（= 名义上限的 80%，该喊了）会被算成 111% ——
//	方向反了还好说，真正糟的是反过来：用偏小的 used 配偏小的 limit，
//	看门狗会在进程逼近 50 MiB 时**保持沉默**。
func TestWarnThresholdUsesJetsamCap(t *testing.T) {
	// 40 MiB = 50 MiB 的 80% —— 正好该喊
	if warn, _ := memWarnDecision(40<<20, neMemoryCapBytes, true); !warn {
		t.Fatal("进程 RSS 到 40 MiB（名义上限的 80%%）就该喊了")
	}
	// 30 MiB = 60% —— 不该喊
	if warn, _ := memWarnDecision(30<<20, neMemoryCapBytes, true); warn {
		t.Fatal("30 MiB 还不该喊")
	}
	if neMemoryCapBytes != 50<<20 {
		t.Fatalf("名义上限被改动了：%d", neMemoryCapBytes)
	}
}

// 读得到一个像样的数（不为 0，也不至于荒谬）。
func TestGoMemoryBytes(t *testing.T) {
	got := goMemoryBytes()
	if got == 0 {
		t.Fatal("读不到内存占用 —— metrics 名字写错时就是这个表现，而且零报错")
	}
	if got > 8<<30 {
		t.Fatalf("值荒谬（%d），八成是 total/released 减反了", got)
	}
}

// 采样周期按占用自适应（2026-08 电池专项）：近上限 2s、余量大 10s。
//
// ★ 钉两头 + 边界：漏现场（近限还慢）与白醒（空闲还快）两个方向的坏都挡住。
func TestMemPeriodDecision(t *testing.T) {
	const limit = 50 << 20
	if d := memPeriodDecision(45<<20, limit); d != memPeriodNear {
		t.Fatalf("九成占用还在远档（%v）—— 会漏掉 jetsam 现场", d)
	}
	if d := memPeriodDecision(30<<20, limit); d != memPeriodNear {
		t.Fatalf("恰在 memNearAt（六成）上必须已是近档，得到 %v", d)
	}
	if d := memPeriodDecision(20<<20, limit); d != memPeriodFar {
		t.Fatalf("四成占用还在近档（%v）—— 空闲隧道整夜白醒", d)
	}
	if d := memPeriodDecision(10<<20, 0); d != memPeriodFar {
		t.Fatalf("读不到上限时该用远档（采样拿不到数据，快跑无益），得到 %v", d)
	}
}

// 峰值只在**创新高且涨够一步**时记。
//
// ★★ 判据的两半都要：只判"新高"的话，缓慢爬升会每 2 秒记一行，
//
//	把只有 100 行的日志页刷空 —— 而那一页正是我们唯一能看到现场的地方。
func TestMemPeakDecision(t *testing.T) {
	var last uint64
	// 第一次：0 → 10 MiB，涨够一步，记
	log1, last := memPeakDecision(10<<20, last)
	if !log1 || last != 10<<20 {
		t.Fatalf("第一次该记，得到 %v / %d", log1, last)
	}
	// 回落：不记，也不能把 last 拉低（否则再涨回去会重复记）
	log2, last := memPeakDecision(6<<20, last)
	if log2 || last != 10<<20 {
		t.Fatalf("回落不该记且不该改 last，得到 %v / %d", log2, last)
	}
	// 小幅新高（+2 MiB，不够一步）：不记
	log3, last := memPeakDecision(12<<20, last)
	if log3 || last != 10<<20 {
		t.Fatalf("涨不够一步不该记，得到 %v / %d", log3, last)
	}
	// 涨够一步（+4 MiB）：记
	log4, last := memPeakDecision(14<<20, last)
	if !log4 || last != 14<<20 {
		t.Fatalf("涨够一步该记，得到 %v / %d", log4, last)
	}
}

// 采样周期必须够密 —— 30 秒会漏掉测速那种十几秒内拉满的现场。
// 自适应之后这条不变量拆成两半：近档管"峰值附近必须密"，
// 远档管"从六成以下冲到被杀（至少 +20 MB、最陡也要十几秒）至少采到一次、
// 好切回近档" —— 远档超过 10 秒就接不住最陡的坡了。
func TestMemPeriodIsDenseEnough(t *testing.T) {
	if memPeriodNear > 5*time.Second {
		t.Fatalf("近档采样周期 %v 太稀，会漏掉峰值现场", memPeriodNear)
	}
	if memPeriodFar > 10*time.Second {
		t.Fatalf("远档采样周期 %v 太稀，最陡的坡上一次都采不到就切不回近档", memPeriodFar)
	}
}

// stderr 真的被接管了 —— 而且 panic 那条路留得下痕迹。
//
// ★★ 这一条必须**真的写一行再读回来**，不能只断言"函数没报错"：
//
//	`dup2` 失败、路径不可写、或者以后有人把追加改成覆盖，
//	都会让文件在**需要它的那一刻**是空的 —— 而那一刻我们已经拿不到第二次机会了。
func TestCaptureStderr(t *testing.T) {
	dir := t.TempDir()

	// ★ 先把真的 stderr 存一份，跑完还回去 —— 否则后面的测试输出会被写进临时文件里，
	//   失败信息就看不见了（这一条我差点漏掉）。
	saved, err := syscall.Dup(int(os.Stderr.Fd()))
	if err != nil {
		t.Skipf("这个平台上 dup 不了 stderr：%v", err)
	}
	defer syscall.Dup2(saved, int(os.Stderr.Fd()))
	defer syscall.Close(saved)

	captureStderr(dir)
	fmt.Fprintln(os.Stderr, "panic: 假装这是一次崩溃")

	data, err := os.ReadFile(filepath.Join(dir, "tunnel-stderr.log"))
	if err != nil {
		t.Fatalf("stderr 文件没建起来：%v", err)
	}
	body := string(data)
	if !strings.Contains(body, "coast tunnel start") {
		t.Fatalf("少了那行开始标记（文件是追加的，没有它分不清哪一次跑的）：%q", body)
	}
	if !strings.Contains(body, "假装这是一次崩溃") {
		t.Fatalf("写到 stderr 的东西没进文件 —— dup2 没生效：%q", body)
	}
}

// 超过上界就重来，别把用户的存储写满。
func TestCaptureStderrTruncatesHugeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tunnel-stderr.log")
	if err := os.WriteFile(path, make([]byte, 300<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	saved, err := syscall.Dup(int(os.Stderr.Fd()))
	if err != nil {
		t.Skipf("dup 不了：%v", err)
	}
	defer syscall.Dup2(saved, int(os.Stderr.Fd()))
	defer syscall.Close(saved)

	captureStderr(dir)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() > 64<<10 {
		t.Fatalf("超过上界没有重来，现在 %d 字节", st.Size())
	}
}

// footprint —— jetsam 真正记账的那个数，读得到且不荒谬。
//
// ★★ 背景（2026-08-21 真机）：看门狗按 RSS 报 92 MiB 而进程活着，
//
//	内核却在 footprint 到 50 时杀（`ActiveHard 50 MB (fatal)`，一晚 6 次）。
//	RSS 含干净 text 页与 MADV_FREE 待回收页，天然高估 —— 判据必须换 footprint。
func TestProcessFootprintBytes(t *testing.T) {
	got := processFootprintBytes()
	if got == 0 {
		t.Fatal("footprint 读不到 —— proc_pid_rusage 失败时就是这个表现，而且零报错")
	}
	if got > 1<<40 {
		t.Fatalf("值荒谬（%d），八成是字段取错了", got)
	}
	// ★ footprint（只算脏页+压缩）不该比 RSS 大出数量级；
	//   两个读数同时拿，能互相兜住"取错字段"这类错。
	if rssLike := goMemoryBytes(); got > 0 && rssLike > 0 && got > rssLike*64 {
		t.Fatalf("footprint(%d) 比 Go 侧读数(%d)大得离谱", got, rssLike)
	}
}

// 上限估计：available 可用时按系统现算，不可用（macOS 宿主 / 读取失败）退回名义值。
//
// ★★ **0 绝不能当上限**：那会让比例除法把每一拍都算成"超限"，
//
//	日志页被自己的告警刷空 —— 比不喊更糟（R151 那条迟滞注释同一个理由）。
func TestMemLimitEstimate(t *testing.T) {
	if got := memLimitEstimate(10<<20, 5<<20); got != 15<<20 {
		t.Fatalf("available 可用时应为 footprint+available，得到 %d", got)
	}
	if got := memLimitEstimate(10<<20, 0); got != neMemoryCapBytes {
		t.Fatalf("available 读不到时应退回名义上限 50 MiB，得到 %d", got)
	}
	// macOS 宿主上 os_proc_available_memory 恒 0 —— 只验"不崩且不荒谬"。
	if a := processAvailableBytes(); a > 1<<40 {
		t.Fatalf("available 荒谬（%d）", a)
	}
}
