package tuic

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	N "github.com/ClashrAuto/coast/common/net"
	C "github.com/ClashrAuto/coast/constant"

	list "github.com/bahlo/generic-list-go"
)

type PoolClient struct {
	newClientOptionV4 *ClientOptionV4
	newClientOptionV5 *ClientOptionV5

	dialFn          DialFunc
	tcpClients      list.List[Client]
	tcpClientsMutex sync.Mutex
	udpClients      list.List[Client]
	udpClientsMutex sync.Mutex
}

func (t *PoolClient) DialContext(ctx context.Context, metadata *C.Metadata) (net.Conn, error) {
	conn, err := t.getClient(false).DialContext(ctx, metadata)
	if errors.Is(err, TooManyOpenStreams) {
		conn, err = t.newClient(false).DialContext(ctx, metadata)
	}
	if err != nil {
		return nil, err
	}
	return N.NewRefConn(conn, t), err
}

func (t *PoolClient) ListenPacket(ctx context.Context, metadata *C.Metadata) (net.PacketConn, error) {
	pc, err := t.getClient(true).ListenPacket(ctx, metadata)
	if errors.Is(err, TooManyOpenStreams) {
		pc, err = t.newClient(true).ListenPacket(ctx, metadata)
	}
	if err != nil {
		return nil, err
	}
	return N.NewRefPacketConn(pc, t), nil
}

func (t *PoolClient) newClient(udp bool) (client Client) {
	clients := &t.tcpClients
	clientsMutex := &t.tcpClientsMutex
	if udp {
		clients = &t.udpClients
		clientsMutex = &t.udpClientsMutex
	}

	clientsMutex.Lock()
	defer clientsMutex.Unlock()

	if t.newClientOptionV4 != nil {
		client = NewClientV4(t.newClientOptionV4, udp, t.dialFn)
	} else {
		client = NewClientV5(t.newClientOptionV5, udp, t.dialFn)
	}

	client.SetLastVisited(time.Now())

	clients.PushFront(client)
	return client
}

func (t *PoolClient) getClient(udp bool) Client {
	clients := &t.tcpClients
	clientsMutex := &t.tcpClientsMutex
	if udp {
		clients = &t.udpClients
		clientsMutex = &t.udpClientsMutex
	}
	var bestClient Client

	func() {
		clientsMutex.Lock()
		defer clientsMutex.Unlock()
		for it := clients.Front(); it != nil; {
			client := it.Value
			if client == nil {
				next := it.Next()
				clients.Remove(it)
				it = next
				continue
			}
			if bestClient == nil {
				bestClient = client
			} else {
				if client.OpenStreams() < bestClient.OpenStreams() {
					bestClient = client
				}
			}
			it = it.Next()
		}
		for it := clients.Front(); it != nil; {
			client := it.Value
			if client != bestClient && client.OpenStreams() == 0 && time.Now().Sub(client.LastVisited()) > 30*time.Minute {
				client.Close()
				next := it.Next()
				clients.Remove(it)
				it = next
				continue
			}
			it = it.Next()
		}
	}()

	if bestClient == nil {
		return t.newClient(udp)
	} else {
		bestClient.SetLastVisited(time.Now())
		return bestClient
	}
}

// Reset 把池里的客户端全部摘掉并关闭（Coast：醒来对账用，见 tunnel/wake.go）。
// 每个客户端攥着一条 QUIC 连接：没有在途流的立刻关，有的等流走完再关（clientImpl.Close
// 的既有语义）。池空了之后下一次拨号由 getClient 新建客户端、重新握手——不是终态。
// ★ 已摘掉的客户端不会回到池里：只有 newClient 往池里放，流结束不会把客户端放回来。
func (t *PoolClient) Reset() {
	for _, pool := range []struct {
		clients *list.List[Client]
		mu      *sync.Mutex
	}{
		{&t.tcpClients, &t.tcpClientsMutex},
		{&t.udpClients, &t.udpClientsMutex},
	} {
		pool.mu.Lock()
		var old []Client
		for it := pool.clients.Front(); it != nil; it = it.Next() {
			if it.Value != nil {
				old = append(old, it.Value)
			}
		}
		pool.clients.Init()
		pool.mu.Unlock()
		// 锁外关：Close 可能要做 QUIC 层的收尾，别攥着池锁挡住新拨号。
		for _, c := range old {
			c.Close()
		}
	}
}

func NewPoolClientV4(clientOption *ClientOptionV4, dialFn DialFunc) *PoolClient {
	p := &PoolClient{
		dialFn: dialFn,
	}
	newClientOption := *clientOption
	p.newClientOptionV4 = &newClientOption
	return p
}

func NewPoolClientV5(clientOption *ClientOptionV5, dialFn DialFunc) *PoolClient {
	p := &PoolClient{
		dialFn: dialFn,
	}
	newClientOption := *clientOption
	p.newClientOptionV5 = &newClientOption
	return p
}
