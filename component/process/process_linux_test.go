//go:build linux

package process

import (
	"net/netip"
	"testing"
)

// netlink 回来的地址不带 zone，metadata.SrcIP 对 fe80::/10 带 zone。
// 两边必须先归一化再比，否则链路本地的源地址永远匹配不上，
// resolveSocketByNetlink 会退到 dump 里最后一条 socket 上去。
func TestSameSocketAddr(t *testing.T) {
	tests := []struct {
		name string
		a    string // netlink 侧（永远无 zone）
		b    string // metadata.SrcIP 侧（链路本地会带 zone）
		want bool
	}{
		{"link-local with zone matches the same address without zone",
			"fe80::1", "fe80::1%eth0", true},
		{"link-local different zone still matches on address",
			"fe80::1", "fe80::1%wlan0", true},
		{"different link-local addresses do not match",
			"fe80::1", "fe80::2%eth0", false},
		{"plain v6 matches",
			"2606:4700:4700::1111", "2606:4700:4700::1111", true},
		{"different v6 does not match",
			"2606:4700:4700::1111", "2606:4700:4700::1001", false},
		{"v4 matches",
			"192.168.1.2", "192.168.1.2", true},
		{"v4-mapped matches plain v4",
			"192.168.1.2", "::ffff:192.168.1.2", true},
		{"v4 does not match a different v4",
			"192.168.1.2", "192.168.1.3", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := netip.MustParseAddr(tt.a)
			b := netip.MustParseAddr(tt.b)
			if got := sameSocketAddr(a, b); got != tt.want {
				t.Fatalf("sameSocketAddr(%s, %s) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
			// 比较必须是对称的，调用方两侧的来源不固定
			if got := sameSocketAddr(b, a); got != tt.want {
				t.Fatalf("sameSocketAddr(%s, %s) = %v, want %v (not symmetric)", tt.b, tt.a, got, tt.want)
			}
		})
	}
}
