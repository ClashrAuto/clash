package resolver

import (
	"context"
	"net/netip"
	"testing"

	"github.com/ClashrAuto/coast/component/trie"
)

// hosts 里的 v6 条目原来绕过了全局 ipv6 开关：LookupIPv4WithResolver 会过滤、
// LookupIPv6WithResolver 会直接拒绝，只有 LookupIPWithResolver 整份返回，
// 于是 ipv6: false 时照样会拿着 AAAA 去拨号。
func TestLookupIPWithResolverRespectsDisableIPv6ForHosts(t *testing.T) {
	oldHosts, oldDisable := DefaultHosts, DisableIPv6
	defer func() { DefaultHosts, DisableIPv6 = oldHosts, oldDisable }()

	value, err := NewHostValue([]string{"1.2.3.4", "2606:4700:4700::1111"})
	if err != nil {
		t.Fatalf("NewHostValue: %v", err)
	}
	tr := trie.New[HostValue]()
	if err := tr.Insert("dual.test", value); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	DefaultHosts = NewHosts(tr)

	t.Run("ipv6 disabled keeps only v4", func(t *testing.T) {
		DisableIPv6 = true
		ips, err := LookupIPWithResolver(context.Background(), "dual.test", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(ips) != 1 || ips[0].String() != "1.2.3.4" {
			t.Fatalf("got %v, want [1.2.3.4] — hosts 绕过了 DisableIPv6", ips)
		}
	})

	t.Run("ipv6 enabled keeps both", func(t *testing.T) {
		DisableIPv6 = false
		ips, err := LookupIPWithResolver(context.Background(), "dual.test", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var hasV4, hasV6 bool
		for _, ip := range ips {
			if ip.Unmap().Is4() {
				hasV4 = true
			} else {
				hasV6 = true
			}
		}
		if !hasV4 || !hasV6 {
			t.Fatalf("got %v, want both families", ips)
		}
	})
}

// 顺带钉住 LookupIPv4WithResolver 的既有行为，作为上面那条的对照组。
func TestLookupIPv4WithResolverFiltersHosts(t *testing.T) {
	oldHosts := DefaultHosts
	defer func() { DefaultHosts = oldHosts }()

	value, err := NewHostValue([]string{"1.2.3.4", "2606:4700:4700::1111"})
	if err != nil {
		t.Fatalf("NewHostValue: %v", err)
	}
	tr := trie.New[HostValue]()
	if err := tr.Insert("dual.test", value); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	DefaultHosts = NewHosts(tr)

	ips, err := LookupIPv4WithResolver(context.Background(), "dual.test", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ips) != 1 || ips[0] != netip.MustParseAddr("1.2.3.4") {
		t.Fatalf("got %v, want [1.2.3.4]", ips)
	}
}
