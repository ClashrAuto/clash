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

// 量 **QUIC 接收窗口盖帽**到底省了多少内存 —— R163 的那个修复此前只有推理，没有数。
//
// ★★★ 做法：让**同一个核心进程**既当 hysteria2 服务端（`listeners:` 里的
//
//	`sing_hysteria2`）又当客户端（一个指向 127.0.0.1 的 hysteria2 出站）。
//	流量从混合端口进 → 命中规则走那个 hysteria2 出站 → 绕回本进程的 hysteria2 监听
//	→ 再直连本地回显服务器。**QUIC 客户端那条路因此被真实地跑起来了**，
//	而 `memprobe_test.go` 里那个纯 TCP 探针**跑不到**它 —— 这正是当初测不出问题的原因。
//
// ⚠️ 一个进程里同时有收发两侧，所以绝对值**不是**手机上的值；
//
//	  有意义的是**同一套 harness 下「盖帽 / 不盖帽」两次跑的差**。
//
//		COAST_QUICPROBE=1 COAST_QUIC_CAP=1 go test ./mobile -run TestQuicWindowMemory -v -timeout 400s
//		COAST_QUICPROBE=1 COAST_QUIC_CAP=0 go test ./mobile -run TestQuicWindowMemory -v -timeout 400s
func TestQuicWindowMemory(t *testing.T) {
	if os.Getenv("COAST_QUICPROBE") == "" {
		t.Skip("设 COAST_QUICPROBE=1 才跑")
	}
	certDir := os.Getenv("COAST_QUIC_CERTDIR")
	if certDir == "" {
		certDir = "/tmp/quicprobe"
	}

	// ★ 载荷要**足够大**：窗口只有在"发得比收得快"时才会被填满。
	//   256 KiB 时一个请求还没爬到窗口就结束了 —— 实测 RTT=200ms 下 RSS 与 RTT=0 无差别。
	payloadMiB := 8
	if v := os.Getenv("COAST_QUIC_PAYLOAD_MIB"); v != "" {
		fmt.Sscanf(v, "%d", &payloadMiB)
	}
	payload := make([]byte, payloadMiB<<20)
	// 每个节点一个回显端口（规则按 DST-PORT 分流）。
	nodeCount := 20
	if v := os.Getenv("COAST_QUIC_NODES"); v != "" {
		fmt.Sscanf(v, "%d", &nodeCount)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(payload) })
	var echoPorts []int
	var targets []string
	for i := 0; i < nodeCount; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: handler}
		go srv.Serve(ln)
		defer srv.Close()
		p := ln.Addr().(*net.TCPAddr).Port
		echoPorts = append(echoPorts, p)
		targets = append(targets, fmt.Sprintf("http://127.0.0.1:%d/", p))
	}

	// ★ 盖帽与否只差这四行 —— 与 `TunConfig.applyQuicReceiveWindowCaps` 盖的是同一组键。
	caps := ""
	if os.Getenv("COAST_QUIC_CAP") == "1" {
		caps = "    initial-stream-receive-window: 2097152\n" +
			"    max-stream-receive-window: 2097152\n" +
			"    initial-connection-receive-window: 4194304\n" +
			"    max-connection-receive-window: 4194304\n"
	}

	// ★ 造 RTT：默认 0（直连监听），设 COAST_QUIC_RTT_MS 就插一个加延迟的 UDP 中继。
	//   窗口的影响只在 RTT 大时才显现 —— 见 `startDelayRelay` 上方那段。
	proxyPort := "18443"
	rttMS := 0
	if v := os.Getenv("COAST_QUIC_RTT_MS"); v != "" {
		fmt.Sscanf(v, "%d", &rttMS)
	}
	if rttMS > 0 {
		addr := startDelayRelay(t, "127.0.0.1:18443", time.Duration(rttMS/2)*time.Millisecond)
		_, p, _ := net.SplitHostPort(addr)
		proxyPort = p
	}

	dir := t.TempDir()
	conf := filepath.Join(dir, "full.yaml")
	// ★★★ **N 个出站节点 = N 条并发的 QUIC 连接。** 这才是把 50 MiB 顶穿的形态：
	//   窗口是**每条连接各自**的上限（hy2 8/20 MiB、tuic 15/64 MiB），
	//   一条连接顶不到帽子（在途量 ≈ BDP），**几十条一起**才顶得到。
	//   用户订阅里是 19 个 hy2 + 10 个 tuic —— R167 那个单出站探针在结构上就复现不了。
	//   这里让每个节点都指向同一个监听（省事），靠 `DST-PORT` 规则把流量分到各自的节点上。
	nodes := 20
	if v := os.Getenv("COAST_QUIC_NODES"); v != "" {
		fmt.Sscanf(v, "%d", &nodes)
	}
	var proxiesYaml, rulesYaml string
	for i := 0; i < nodes; i++ {
		proxiesYaml += fmt.Sprintf(
			"  - name: HY2-%d\n    type: hysteria2\n    server: 127.0.0.1\n    port: %s\n"+
				"    password: probe:probepass\n    sni: localhost\n    skip-cert-verify: true\n",
			i, proxyPort) + caps
		rulesYaml += fmt.Sprintf("  - DST-PORT,%d,HY2-%d\n", echoPorts[i], i)
	}
	yaml := "mixed-port: 17891\nmode: rule\nlog-level: silent\n" +
		"listeners:\n" +
		"  - name: hy2-in\n    type: hysteria2\n    port: 18443\n    listen: 127.0.0.1\n" +
		"    users:\n      probe: probepass\n" +
		"    certificate: " + filepath.Join(certDir, "cert.pem") + "\n" +
		"    private-key: " + filepath.Join(certDir, "key.pem") + "\n" +
		"proxies:\n" + proxiesYaml +
		"proxy-groups: []\n" +
		"rules:\n" + rulesYaml + "  - MATCH,DIRECT\n"
	if err := os.WriteFile(conf, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := startCore(dir, conf); err != nil {
		t.Fatalf("核心起不来：%v", err)
	}
	defer stopCore()
	time.Sleep(2500 * time.Millisecond)

	proxyURL, _ := url.Parse("http://127.0.0.1:17891")
	rss := func() float64 { return float64(processRSSBytes()) / (1 << 20) }

	fmt.Printf("CAP=%s RTT=%dms IDLE_RSS=%.1f\n", os.Getenv("COAST_QUIC_CAP"), rttMS, rss())

	var ok, fail int
	var mu sync.Mutex
	for _, conns := range []int{nodeCount} {
		var wg sync.WaitGroup
		stop := make(chan struct{})
		for i := 0; i < conns; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				c := &http.Client{
					Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
					Timeout:   15 * time.Second,
				}
				for {
					select {
					case <-stop:
						return
					default:
					}
					resp, err := c.Get(targets[idx%len(targets)])
					mu.Lock()
					if err != nil {
						fail++
					} else {
						ok++
					}
					mu.Unlock()
					if err != nil {
						time.Sleep(200 * time.Millisecond)
						continue
					}
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}(i)
		}
		time.Sleep(12 * time.Second)
		peak := rss()
		close(stop)
		wg.Wait()
		fmt.Printf("CAP=%s RTT=%dms CONNS=%d PEAK_RSS=%.1f OK=%d FAIL=%d\n",
			os.Getenv("COAST_QUIC_CAP"), rttMS, conns, peak, ok, fail)
	}
	if ok == 0 {
		t.Fatalf("一次都没成功 —— QUIC 那条路根本没跑起来，这次测量没有意义（fail=%d）", fail)
	}
}

// startDelayRelay 起一个**加延迟的 UDP 中继**：客户端打到它，它把包转给上游，
// 两个方向各压 `delay` —— 于是 RTT ≈ 2×delay。
//
// ★★★ **它存在的理由**：接收窗口的作用是覆盖「带宽 × RTT」。回环 RTT ≈ 0 时 BDP 趋近 0，
//
//	窗口**永远填不满**，2 MiB 和 8 MiB 完全没区别 —— R166 就是这么白测一轮的。
//	要量出窗口的影响，必须先把 RTT 造出来。
//
// ★ 纯用户态，不需要 sudo（`dnctl`/`pfctl` 那条路要）。
//
// ⚠️ **每个包各自计时，不能在转发路径上直接 sleep**：那样会把包**串行化**
//
//	（第 N 个包要等前 N-1 个的 sleep），变成人为的带宽限制而不是延迟 ——
//	量出来的就不是窗口的影响了。所以用 `time.AfterFunc` 一包一个定时器。
func startDelayRelay(t *testing.T, upstream string, delay time.Duration) string {
	t.Helper()
	up, err := net.ResolveUDPAddr("udp", upstream)
	if err != nil {
		t.Fatal(err)
	}
	client, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })

	var mu sync.Mutex
	// 每个客户端地址一条到上游的 socket，并把上游回包按同样的延迟送回去。
	conns := map[string]net.PacketConn{}

	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, err := client.ReadFrom(buf)
			if err != nil {
				return
			}
			key := addr.String()
			mu.Lock()
			uc, ok := conns[key]
			if !ok {
				uc, err = net.ListenPacket("udp", "127.0.0.1:0")
				if err != nil {
					mu.Unlock()
					return
				}
				conns[key] = uc
				go func(uc net.PacketConn, back net.Addr) {
					b := make([]byte, 65535)
					for {
						n, _, err := uc.ReadFrom(b)
						if err != nil {
							return
						}
						p := make([]byte, n)
						copy(p, b[:n])
						time.AfterFunc(delay, func() { client.WriteTo(p, back) })
					}
				}(uc, addr)
			}
			mu.Unlock()
			p := make([]byte, n)
			copy(p, buf[:n])
			time.AfterFunc(delay, func() { uc.WriteTo(p, up) })
		}
	}()
	return client.LocalAddr().String()
}
