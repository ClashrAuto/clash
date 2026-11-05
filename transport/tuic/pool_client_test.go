package tuic

import (
	"context"
	"net"
	"testing"
	"time"

	C "github.com/ClashrAuto/coast/constant"
)

type fakeTuicClient struct {
	closed bool
}

func (f *fakeTuicClient) DialContext(context.Context, *C.Metadata) (net.Conn, error) {
	return nil, nil
}
func (f *fakeTuicClient) ListenPacket(context.Context, *C.Metadata) (net.PacketConn, error) {
	return nil, nil
}
func (f *fakeTuicClient) OpenStreams() int64         { return 0 }
func (f *fakeTuicClient) LastVisited() time.Time     { return time.Now() }
func (f *fakeTuicClient) SetLastVisited(_ time.Time) {}
func (f *fakeTuicClient) Close()                     { f.closed = true }

// Reset 把池里的客户端（各攥一条 QUIC 连接）全部关掉并摘掉；之后池是空的，
// 下一次拨号会新建客户端——这正是醒来对账要的「不复用睡前的连接」。
func TestPoolClientResetClosesAndEmpties(t *testing.T) {
	p := &PoolClient{}
	tcp1, tcp2, udp := &fakeTuicClient{}, &fakeTuicClient{}, &fakeTuicClient{}
	p.tcpClients.PushFront(tcp1)
	p.tcpClients.PushFront(tcp2)
	p.udpClients.PushFront(udp)

	p.Reset()

	for i, c := range []*fakeTuicClient{tcp1, tcp2, udp} {
		if !c.closed {
			t.Fatalf("第 %d 个客户端没被关掉", i)
		}
	}
	if p.tcpClients.Len() != 0 || p.udpClients.Len() != 0 {
		t.Fatalf("Reset 之后池应为空，得到 tcp=%d udp=%d", p.tcpClients.Len(), p.udpClients.Len())
	}
	// 再 Reset 一次是空操作（醒来对账可能对同一个出站喊多次）。
	p.Reset()
}
