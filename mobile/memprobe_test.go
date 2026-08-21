//go:build darwin && cgo

package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// 量一量核心自己到底吃多少内存 —— **不靠推，靠跑**。
//
// ★★★ 为什么需要它：iOS 上那 50 MiB 的 jetsam 上限我们只能猜有多少余量，
//
//	而真机上除了"被杀了"之外拿不到任何数。这个探针在**本机**把核心真起来、
//	真压上并发连接，量出「静息 RSS」和「并发下的峰值」——
//	有了这两个数，`COAST_GOMEMLIMIT` 该设多少就是**算得出来**的，不用再拍脑袋。
//
// ⚠️ 与 iOS 的差别要认：本机是 macOS，没有 NE 扩展那层框架开销，
//
//	Go 之外的基线会不一样。但**Go 这半边的量级和增长曲线是可迁移的**，
//	而那正是要判断的东西。
//
// 跑法（默认跳过，免得拖慢正常的 go test）：
//
//	COAST_MEMPROBE=1 go test ./mobile -run TestMemoryUnderLoad -v -timeout 300s
func TestMemoryUnderLoad(t *testing.T) {
	if os.Getenv("COAST_MEMPROBE") == "" {
		t.Skip("设 COAST_MEMPROBE=1 才跑（它要起真核心并压并发）")
	}

	// 本地回显服务器当"目标站点" —— 不碰真网络，量的才是核心自己。
	payload := make([]byte, 64<<10)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	})}
	go srv.Serve(ln)
	defer srv.Close()
	target := "http://" + ln.Addr().String() + "/"

	dir := t.TempDir()
	// ★ 最小配置：**不带 GEOIP** —— 那会去找 Country.mmdb 然后下载超时，
	//   而那个超时读起来像配置错误（仓库里记过这一跤）。
	conf := filepath.Join(dir, "full.yaml")
	if err := os.WriteFile(conf, []byte(
		"mixed-port: 17890\nmode: rule\nlog-level: silent\n"+
			"proxies: []\nproxy-groups: []\nrules:\n  - MATCH,DIRECT\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// ★ 走纯 Go 的 `startCore`，不过 C ABI —— cgo 在测试文件里用不了。
	if err := startCore(dir, conf); err != nil {
		t.Fatalf("核心起不来：%v", err)
	}
	defer stopCore()
	time.Sleep(1500 * time.Millisecond) // 等监听器真的绑上

	proxyURL, _ := url.Parse("http://127.0.0.1:17890")
	rssMiB := func() float64 { return float64(processRSSBytes()) / (1 << 20) }
	goMiB := func() float64 { return float64(goMemoryBytes()) / (1 << 20) }

	idle := rssMiB()
	fmt.Printf("IDLE_RSS=%.1f IDLE_GO=%.1f\n", idle, goMiB())

	for _, conns := range []int{8, 32, 128} {
		var wg sync.WaitGroup
		stop := make(chan struct{})
		for i := 0; i < conns; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c := &http.Client{
					Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
					Timeout:   10 * time.Second,
				}
				for {
					select {
					case <-stop:
						return
					default:
					}
					resp, err := c.Get(target)
					if err != nil {
						return
					}
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}()
		}
		time.Sleep(6 * time.Second)
		peak, gp := rssMiB(), goMiB()
		close(stop)
		wg.Wait()
		time.Sleep(2 * time.Second)
		fmt.Printf("CONNS=%d PEAK_RSS=%.1f PEAK_GO=%.1f AFTER=%.1f\n",
			conns, peak, gp, rssMiB())
	}
}
