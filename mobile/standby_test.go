//go:build darwin && cgo

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClashrAuto/coast/adapter/provider"
	"github.com/ClashrAuto/coast/listener/inbound"
	"github.com/ClashrAuto/coast/tunnel"
	"github.com/ClashrAuto/tide"
)

// meter 夹在客户端与 tide 服务端之间的计量器：线路上有几条连接、走过多少字节。
//
// ★ 判据放在线路上，不去数 goroutine：服务端也在这个进程里，数 goroutine 分不清两边，
// 而这个测试要回答的问题就是「锁屏之后线上还有没有包」。
type meter struct {
	ln     net.Listener
	target string
	active atomic.Int32
	opened atomic.Int32
	bytes  atomic.Int64
	wg     sync.WaitGroup
}

func newMeter(t *testing.T, target string) *meter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &meter{ln: ln, target: target}
	go m.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return m
}

func (m *meter) serve() {
	for {
		c, err := m.ln.Accept()
		if err != nil {
			return
		}
		up, err := net.Dial("tcp", m.target)
		if err != nil {
			_ = c.Close()
			continue
		}
		m.opened.Add(1)
		m.active.Add(1)
		var once sync.Once
		done := func() {
			once.Do(func() {
				m.active.Add(-1)
				_ = c.Close()
				_ = up.Close()
			})
		}
		pipe := func(dst, src net.Conn) {
			defer done()
			n, _ := io.Copy(countingWriter{dst, &m.bytes}, src)
			_ = n
		}
		go pipe(up, c)
		go pipe(c, up)
	}
}

type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

func selfSigned(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "tide.test"},
		DNSNames:     []string{"tide.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	kd, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}))
}

type standbyRig struct {
	full, bare string
	wire       *meter
	testURL    string
	server     *inbound.Tide
}

// newStandbyRig 起一台真的 tide 服务端（不经配置，所以不会被 ApplyConfig 带走）、一个测速目标，
// 再把内核按带 tide 节点的配置起来。
func newStandbyRig(t *testing.T) *standbyRig {
	t.Helper()
	provider.SetOnDemandHealthCheck(true, nil)
	t.Cleanup(func() { provider.SetOnDemandHealthCheck(false, nil) })

	hits := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: hits}
	go srv.Serve(targetLn)
	t.Cleanup(func() { _ = srv.Close() })
	coverLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cover := &http.Server{Handler: hits}
	go cover.Serve(coverLn)
	t.Cleanup(func() { _ = cover.Close() })

	priv, err := tide.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	cert, key := selfSigned(t)
	portLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := portLn.Addr().(*net.TCPAddr).Port
	_ = portLn.Close()
	in, err := inbound.NewTide(&inbound.TideOption{
		// 服务端与客户端在同一个进程里，共用规则表；不指定的话流量进来又走回 tide 节点，绕成环。
		BaseOption:    inbound.BaseOption{NameStr: "tide-server", Listen: "127.0.0.1", Port: fmt.Sprint(port), SpecialProxy: "DIRECT"},
		Users:         map[string]string{"phone": "pw"},
		PrivateKey:    priv.String(),
		Certificate:   cert,
		PrivateKeyPEM: key,
		Cover:         coverLn.Addr().String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Listen(tunnel.Tunnel); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Close() })

	wire := newMeter(t, fmt.Sprintf("127.0.0.1:%d", port))
	wirePort := wire.ln.Addr().(*net.TCPAddr).Port
	testURL := "http://" + targetLn.Addr().String() + "/"

	dir := t.TempDir()
	general := "mixed-port: 17893\nmode: rule\nlog-level: silent\n"
	full := filepath.Join(dir, "full.yaml")
	if err := os.WriteFile(full, []byte(general+fmt.Sprintf(`proxies:
  - name: tide-a
    type: tide
    server: 127.0.0.1
    port: %d
    password: pw
    public-key: %s
    sni: tide.test
    skip-cert-verify: true
proxy-groups:
  - name: AUTO
    type: url-test
    proxies: [tide-a]
    url: %s
    interval: 60
rules:
  - MATCH,AUTO
`, wirePort, priv.Public().String(), testURL)), 0o644); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(dir, "standby.yaml")
	if err := os.WriteFile(bare, []byte(general+"proxies: []\nproxy-groups: []\nrules:\n  - MATCH,DIRECT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := startCore(dir, full); err != nil {
		t.Fatalf("核心起不来：%v", err)
	}
	t.Cleanup(stopCore)
	return &standbyRig{full: full, bare: bare, wire: wire, testURL: testURL, server: in}
}

func (r *standbyRig) probe(t *testing.T) uint16 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, err := tunnel.Proxies()["tide-a"].URLTest(ctx, r.testURL, nil)
	if err != nil {
		t.Fatalf("经 tide 测速失败：%v", err)
	}
	return d
}

func waitInt32(t *testing.T, what string, c *atomic.Int32, want int32, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for c.Load() != want && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := c.Load(); got != want {
		t.Fatalf("%s：应为 %d，实际 %d", what, want, got)
	}
}

// 这个方案成立的前提：节点卸载之后线路上一个字节都没有，也不会再有。
func TestStandbyLeavesTheWireSilent(t *testing.T) {
	r := newStandbyRig(t)

	time.Sleep(300 * time.Millisecond)
	if n := r.wire.opened.Load(); n != 0 {
		t.Fatalf("加载配置不该拨号，线上已经有 %d 条连接", n)
	}

	r.probe(t)
	if r.wire.active.Load() == 0 {
		t.Fatal("测速之后 tide 会话应该还挂着，否则这个测试验不到卸载")
	}

	if err := enterStandby(r.bare); err != nil {
		t.Fatal(err)
	}
	waitInt32(t, "卸载之后线上的连接", &r.wire.active, 0, 2*time.Second)
	if _, ok := tunnel.Proxies()["tide-a"]; ok {
		t.Fatal("节点还在内核里")
	}

	bytes, opened := r.wire.bytes.Load(), r.wire.opened.Load()
	time.Sleep(2 * time.Second)
	if b, o := r.wire.bytes.Load(), r.wire.opened.Load(); b != bytes || o != opened {
		t.Fatalf("待机时线上还有动静：字节 %d→%d，连接 %d→%d", bytes, b, opened, o)
	}
}

// 解锁重新加载：节点回来、测速结果还在、不拨号不测速。
func TestReloadRestoresNodesWithoutDialing(t *testing.T) {
	r := newStandbyRig(t)
	want := r.probe(t)
	if want == 0 {
		t.Fatal("测速结果不该是 0")
	}
	if err := enterStandby(r.bare); err != nil {
		t.Fatal(err)
	}
	waitInt32(t, "卸载之后线上的连接", &r.wire.active, 0, 2*time.Second)
	opened := r.wire.opened.Load()

	if err := reloadCore(r.full); err != nil {
		t.Fatal(err)
	}
	p, ok := tunnel.Proxies()["tide-a"]
	if !ok {
		t.Fatal("节点没有回来")
	}
	if got := p.LastDelayForTestUrl(r.testURL); got != want {
		t.Fatalf("测速结果没有搬过来：应为 %d，实际 %d", want, got)
	}
	time.Sleep(300 * time.Millisecond)
	if n := r.wire.opened.Load(); n != opened {
		t.Fatalf("重新加载不该拨号，线上多了 %d 条连接", n-opened)
	}

	// 回来之后要真的能用。
	r.probe(t)
	if r.wire.opened.Load() == opened {
		t.Fatal("测速没有走到 tide 服务端")
	}
}

// 待机时重复卸载（进休眠重试）不能把留着的测速结果冲成空。
func TestEnterStandbyTwiceKeepsHealth(t *testing.T) {
	r := newStandbyRig(t)
	want := r.probe(t)
	for i := 0; i < 2; i++ {
		if err := enterStandby(r.bare); err != nil {
			t.Fatal(err)
		}
	}
	if err := reloadCore(r.full); err != nil {
		t.Fatal(err)
	}
	if got := tunnel.Proxies()["tide-a"].LastDelayForTestUrl(r.testURL); got != want {
		t.Fatalf("测速结果丢了：应为 %d，实际 %d", want, got)
	}
}

// app 切模式只改内核和 config.yaml，full.yaml 里还是旧的。每次解锁都重载，不沿用内核的值的话
// 用户切到全局之后解锁一次就悄悄变回规则。
func TestStandbyKeepsRuntimeMode(t *testing.T) {
	r := newStandbyRig(t)
	tunnel.SetMode(tunnel.Global)
	t.Cleanup(func() { tunnel.SetMode(tunnel.Rule) })

	if err := enterStandby(r.bare); err != nil {
		t.Fatal(err)
	}
	if got := tunnel.Mode(); got != tunnel.Global {
		t.Fatalf("待机时模式变了：%v", got)
	}
	// 待机期间切模式（解锁了、节点还没加载完）也要算数。
	tunnel.SetMode(tunnel.Direct)
	if err := reloadCore(r.full); err != nil {
		t.Fatal(err)
	}
	if got := tunnel.Mode(); got != tunnel.Direct {
		t.Fatalf("解锁之后模式变了：%v", got)
	}
}

// 运行中的热重载是 app 重建过 full.yaml 之后发的，模式以文件为准，不能被内核的旧值盖掉。
func TestHotReloadTakesModeFromConfig(t *testing.T) {
	r := newStandbyRig(t)
	tunnel.SetMode(tunnel.Global)
	t.Cleanup(func() { tunnel.SetMode(tunnel.Rule) })
	if err := reloadCore(r.full); err != nil {
		t.Fatal(err)
	}
	if got := tunnel.Mode(); got != tunnel.Rule {
		t.Fatalf("热重载应该用配置里的模式，实际 %v", got)
	}
}

// 每锁一次屏就卸载、重建一轮，一天几十轮。这里不能有累积：协程和堆都要回到起点。
func TestStandbyCyclesDoNotLeak(t *testing.T) {
	r := newStandbyRig(t)
	cycle := func() {
		r.probe(t)
		if err := enterStandby(r.bare); err != nil {
			t.Fatal(err)
		}
		waitInt32(t, "卸载之后线上的连接", &r.wire.active, 0, 2*time.Second)
		if err := reloadCore(r.full); err != nil {
			t.Fatal(err)
		}
	}
	heap := func() uint64 {
		runtime.GC()
		debug.FreeOSMemory()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	for i := 0; i < 3; i++ { // 前几轮有一次性的初始化（TLS、QUIC 表、缓存），不算
		cycle()
	}
	first := heap()
	for i := 0; i < 200; i++ {
		cycle()
	}

	// 服务端也在这个进程里，它的会话要等宽限期才退，所以关掉它再数。
	if err := enterStandby(r.bare); err != nil {
		t.Fatal(err)
	}
	_ = r.server.Close()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > 16 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > 16 {
		buf := make([]byte, 1<<18)
		t.Fatalf("协程没有回到起点：%d\n%s", n, buf[:runtime.Stack(buf, true)])
	}
	if last := heap(); last > first+512<<10 {
		t.Fatalf("堆在涨：200 轮之后 %d KiB → %d KiB", first>>10, last>>10)
	}
}

// taskinfo 的写法是「interrupt wakeups: N (M / x% from platform idle)」，两者是包含关系。
func TestIdleWakeupsAreASubset(t *testing.T) {
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	all, idle := taskWakeups(), taskIdleWakeups()
	if all == 0 {
		t.Skip("这台机器读不到 task_power_info")
	}
	if idle > all {
		t.Fatalf("空闲唤醒 %d 多于全部唤醒 %d", idle, all)
	}
}

// 分状态量唤醒次数（默认跳过，它要跑半分钟）：
//
//	COAST_WAKEPROBE=1 go test ./mobile -run TestWakeRateProbe -v -timeout 120s
func TestWakeRateProbe(t *testing.T) {
	if os.Getenv("COAST_WAKEPROBE") == "" {
		t.Skip("设 COAST_WAKEPROBE=1 才跑")
	}
	r := newStandbyRig(t)
	measure := func(what string) {
		w0, t0 := taskWakeups(), time.Now()
		time.Sleep(8 * time.Second)
		rate := float64(taskWakeups()-w0) / time.Since(t0).Seconds()
		fmt.Printf("WAKES %-24s %8.1f /s  goroutines=%d\n", what, rate, runtime.NumGoroutine())
	}
	measure("loaded, no session")
	r.probe(t)
	measure("loaded, tide session idle")
	if err := enterStandby(r.bare); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	measure("standby, server up")
	_ = r.server.Close()
	time.Sleep(time.Second)
	measure("standby, server closed")
	if err := reloadCore(r.full); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	measure("reloaded, no session")

	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	groups := map[string]int{}
	for _, g := range strings.Split(string(buf), "\n\n") {
		lines := strings.Split(g, "\n")
		key := ""
		for _, l := range lines[1:] {
			if strings.HasPrefix(l, "\t") || strings.HasPrefix(l, "created by") {
				continue
			}
			if strings.HasPrefix(l, "runtime.") || strings.HasPrefix(l, "internal/poll") ||
				strings.HasPrefix(l, "time.") || strings.HasPrefix(l, "sync.") ||
				strings.HasPrefix(l, "net.") || strings.HasPrefix(l, "os.") {
				continue
			}
			key = l
			break
		}
		if i := strings.IndexByte(key, '('); i > 0 && !strings.Contains(key[:i], ".(") {
			key = key[:i]
		}
		groups[key]++
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return groups[keys[i]] > groups[keys[j]] })
	for _, k := range keys {
		fmt.Printf("GOROUTINE %3d %s\n", groups[k], k)
	}
}

// 配置读不出来时什么都不动：内核继续带着节点，调用方看得到错误。
func TestEnterStandbyWithBadConfigChangesNothing(t *testing.T) {
	r := newStandbyRig(t)
	if err := enterStandby(filepath.Join(filepath.Dir(r.bare), "no-such.yaml")); err == nil {
		t.Fatal("配置不存在应该报错")
	}
	if _, ok := tunnel.Proxies()["tide-a"]; !ok {
		t.Fatal("配置读失败时不该卸掉节点")
	}
	r.probe(t)
}
