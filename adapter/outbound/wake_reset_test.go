package outbound

import (
	"context"
	"testing"
	"time"

	"github.com/ClashrAuto/tide"
	M "github.com/metacubex/sing/common/metadata"
)

// hysteria2 的重置只能整个换客户端（sing-quic 的 CloseWithError 是终态）。三件事：
// 换了新的、旧的确实关了（再拨立刻失败，不碰网络）、Close 之后不再换新的。
func TestHysteria2ResetTransportSwapsClient(t *testing.T) {
	h, err := NewHysteria2(Hysteria2Option{
		Name:     "hy2-reset",
		Server:   "192.0.2.1", // TEST-NET-1，构造过程不碰网络
		Port:     443,
		Password: "pw",
	})
	if err != nil {
		t.Fatal(err)
	}
	old := h.client.Load()
	h.ResetTransport()
	fresh := h.client.Load()
	if fresh == nil || fresh == old {
		t.Fatal("ResetTransport 应换上一个新客户端")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := old.DialConn(ctx, M.ParseSocksaddrHostPort("example.invalid", 443)); err == nil {
		t.Fatal("被换下的旧客户端应已关闭（再拨立刻失败）")
	}

	_ = h.Close()
	closedClient := h.client.Load()
	h.ResetTransport()
	if h.client.Load() != closedClient {
		t.Fatal("Close 之后不该再换新客户端（否则出站被移除后还漏一个客户端）")
	}
}

// tide 没有会话时重置是空操作：不为它去拨号、不建会话。
func TestTideResetTransportWithoutSessionIsNoop(t *testing.T) {
	priv, err := tide.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	tt, err := NewTide(TideOption{
		Name:      "tide-reset",
		Server:    "192.0.2.1",
		Port:      9,
		Password:  "pw",
		PublicKey: priv.Public().String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tt.Close()
	tt.ResetTransport()
	if tt.client.CurrentSession() != nil {
		t.Fatal("没有会话时 ResetTransport 不该建出一条会话")
	}
}
