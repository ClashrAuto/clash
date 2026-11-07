//go:build darwin && cgo && with_gvisor

package main

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/gvisor/pkg/tcpip"
	"github.com/metacubex/gvisor/pkg/tcpip/stack"
	tun "github.com/metacubex/sing-tun"
	"golang.org/x/sys/unix"
)

// tunWakesPerPacketLimit 稀疏流量下每个包允许的唤醒数。
//
// ★★ 判的是「每包」，不是「每秒」，而且包必须是稀疏的。上游 sing-tun 在 darwin 上用
// 普通的阻塞 kevent 等 tun，P 留在 syscall 状态，sysmon 不能深睡，还要 10ms 后才收回
// 这个 P，其间按 20µs 轮询，于是每次从空闲进入等待都付约 60 次唤醒。包一密读循环就
// 到不了阻塞那一步，所以大流量下看不出来，只有隧道闲着时才现形。
//
// 这个用例量到的基线：阻塞 kevent 54.7 次/包，netpoll 5.5 次/包。
const tunWakesPerPacketLimit = 25

type countingDispatcher struct{ n atomic.Int64 }

func (d *countingDispatcher) DeliverNetworkPacket(tcpip.NetworkProtocolNumber, *stack.PacketBuffer) {
	d.n.Add(1)
}

func (d *countingDispatcher) DeliverLinkPacket(tcpip.NetworkProtocolNumber, *stack.PacketBuffer) {}

// 一个 utun 格式的包：4 字节地址族 + IPv4 + UDP。
func utunUDPPacket() []byte {
	p := make([]byte, 4+20+8)
	p[3] = unix.AF_INET
	ip := p[4:]
	ip[0], ip[3], ip[8], ip[9] = 0x45, 28, 64, 17
	copy(ip[12:16], []byte{172, 19, 0, 1})
	copy(ip[16:20], []byte{172, 19, 0, 2})
	copy(ip[20:], []byte{0x30, 0x39, 0, 9, 0, 8, 0, 0})
	return p
}

// 从 tun 读包的路径在隧道闲着时不能把唤醒放大几十倍，停机时也不能挂住。
// 用 socketpair 冒充 utun，所以不需要 root；走的是产品用的同一个 endpoint。
func TestTunSparsePacketsStayCheap(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	peer := fds[1]
	defer unix.Close(peer)

	tunIf, err := tun.New(tun.Options{FileDescriptor: fds[0], MTU: 1500})
	if err != nil {
		t.Fatal(err)
	}
	defer tunIf.Close()
	gTun, ok := tunIf.(tun.GVisorTun)
	if !ok {
		t.Fatalf("%T 不是 GVisorTun", tunIf)
	}
	ep, _, err := gTun.NewEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	var d countingDispatcher
	ep.Attach(&d)

	const packets = 40
	pkt := utunUDPPacket()
	time.Sleep(200 * time.Millisecond)
	w0 := taskWakeups()
	for i := 0; i < packets; i++ {
		// 间隔要远大于 sysmon 收回 P 的 10ms，每个包才是一次「从空闲进入等待」。
		time.Sleep(100 * time.Millisecond)
		if _, err := unix.Write(peer, pkt); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	perPacket := float64(taskWakeups()-w0) / packets
	t.Logf("%.1f 次唤醒/包（上限 %d）", perPacket, tunWakesPerPacketLimit)

	// 包没送到的话下面那个数没有意义。
	if got := d.n.Load(); got != packets {
		t.Fatalf("送了 %d 个包，收到 %d 个", packets, got)
	}
	if perPacket > tunWakesPerPacketLimit {
		t.Errorf("%.1f 次唤醒/包，上限 %d：读循环又阻塞在 syscall 里了", perPacket, tunWakesPerPacketLimit)
	}

	// Attach(nil) 里 Stop 之后要等读循环退出。等待换到 netpoller 之后，停止管道叫不醒
	// 它，Stop 必须另想办法；叫不醒的话内核重载和停隧道会永远挂在这里。
	stopped := make(chan struct{})
	go func() {
		ep.Attach(nil)
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Attach(nil) 3 秒没返回：读循环没被叫醒")
	}
}
