package listener

import (
	"net"
	"testing"
)

func TestGenAddr(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		port     int
		allowLan bool
		want     string
	}{
		{"lan off ignores host", "0.0.0.0", 7890, false, "127.0.0.1:7890"},
		{"wildcard", "*", 7890, true, ":7890"},
		{"ipv4 literal", "192.168.1.2", 7890, true, "192.168.1.2:7890"},
		{"hostname", "localhost", 7890, true, "localhost:7890"},
		// 修复前这三个会分别拼成 ":::7890" / "fd00::1:7890" / "fe80::1%eth0:7890"，
		// net.SplitHostPort 报 too many colons，监听起不来。
		{"ipv6 unspecified", "::", 7890, true, "[::]:7890"},
		{"ipv6 literal", "fd00::1", 7890, true, "[fd00::1]:7890"},
		{"ipv6 with zone", "fe80::1%eth0", 7890, true, "[fe80::1%eth0]:7890"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := genAddr(tt.host, tt.port, tt.allowLan)
			if got != tt.want {
				t.Fatalf("genAddr(%q, %d, %v) = %q, want %q", tt.host, tt.port, tt.allowLan, got, tt.want)
			}
			// 真正要保证的是它能被解析回去 —— 监听路径上每一处都走 SplitHostPort。
			if _, _, err := net.SplitHostPort(got); err != nil {
				t.Fatalf("genAddr produced an unparsable address %q: %v", got, err)
			}
			if portIsZero(got) {
				t.Fatalf("portIsZero(%q) is true, the listener would be skipped", got)
			}
		})
	}
}
