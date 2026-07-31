package resolver

import (
	"net/netip"
	"testing"
)

func TestLookupIP4P(t *testing.T) {
	SetIP4PEnable(true)
	defer SetIP4PEnable(false)

	tests := []struct {
		name     string
		addr     string
		port     string
		wantAddr string
		wantPort string
	}{
		{
			// 2001:0000:<checksum>:<port>:<v4> —— 端口 0x1f90 = 8080，v4 = 1.2.3.4
			name:     "ip4p is converted",
			addr:     "2001:0:0:0:0:1f90:102:304",
			port:     "443",
			wantAddr: "1.2.3.4",
			wantPort: "8080",
		},
		{
			// 32.1.0.0 的四个字节正好是 20 01 00 00，和 IP4P 前缀撞车。
			// 修复前这里会在 ip[10] 越界 panic（AsSlice() 对 v4 只有 4 字节）。
			name:     "ipv4 colliding with the ip4p prefix is left alone",
			addr:     "32.1.0.0",
			port:     "443",
			wantAddr: "32.1.0.0",
			wantPort: "443",
		},
		{
			// v4-mapped 形式同样只应原样返回
			name:     "v4-in-v6 colliding with the ip4p prefix is left alone",
			addr:     "::ffff:32.1.0.0",
			port:     "443",
			wantAddr: "::ffff:32.1.0.0",
			wantPort: "443",
		},
		{
			name:     "unrelated ipv6 is left alone",
			addr:     "2606:4700:4700::1111",
			port:     "443",
			wantAddr: "2606:4700:4700::1111",
			wantPort: "443",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr, port := LookupIP4P(netip.MustParseAddr(tt.addr), tt.port)
			if addr.String() != tt.wantAddr || port != tt.wantPort {
				t.Fatalf("LookupIP4P(%s, %s) = (%s, %s), want (%s, %s)",
					tt.addr, tt.port, addr, port, tt.wantAddr, tt.wantPort)
			}
		})
	}
}

func TestLookupIP4PDisabled(t *testing.T) {
	SetIP4PEnable(false)

	in := netip.MustParseAddr("2001:0:0:0:0:1f90:102:304")
	addr, port := LookupIP4P(in, "443")
	if addr != in || port != "443" {
		t.Fatalf("ip4p disabled but still converted: got (%s, %s)", addr, port)
	}
}
