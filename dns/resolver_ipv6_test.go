package dns

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/ClashrAuto/coast/component/resolver"

	D "github.com/miekg/dns"
)

// stubClient 让 A / AAAA 两条腿分别可控。
type stubClient struct {
	aErr    error
	aIP     string
	aaaaErr error
	aaaaIP  string
	// AAAA 回答前的停顿，用来制造「A 已失败、AAAA 还没回来」的窗口
	aaaaDelay time.Duration
}

func (s *stubClient) Address() string  { return "stub" }
func (s *stubClient) ResetConnection() {}

func (s *stubClient) ExchangeContext(ctx context.Context, m *D.Msg) (*D.Msg, error) {
	q := m.Question[0]
	switch q.Qtype {
	case D.TypeAAAA:
		if s.aaaaDelay > 0 {
			select {
			case <-time.After(s.aaaaDelay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if s.aaaaErr != nil {
			return nil, s.aaaaErr
		}
		return answer(q, s.aaaaIP), nil
	default:
		if s.aErr != nil {
			return nil, s.aErr
		}
		return answer(q, s.aIP), nil
	}
}

func answer(q D.Question, ip string) *D.Msg {
	msg := &D.Msg{}
	msg.SetQuestion(q.Name, q.Qtype)
	msg.Response = true
	hdr := D.RR_Header{Name: q.Name, Rrtype: q.Qtype, Class: D.ClassINET, Ttl: 60}
	addr := netip.MustParseAddr(ip)
	if addr.Is4() {
		msg.Answer = []D.RR{&D.A{Hdr: hdr, A: addr.AsSlice()}}
	} else {
		msg.Answer = []D.RR{&D.AAAA{Hdr: hdr, AAAA: addr.AsSlice()}}
	}
	return msg
}

// A 查询失败、AAAA 又没能在 ipv6-timeout(默认 100ms) 内回来时，
// 修复前 LookupIP 返回 (空切片, nil) —— 调用方完全看不到失败原因。
func TestLookupIPSurfacesErrorWhenIPv6TimesOut(t *testing.T) {
	r := NewResolverFromClient(&stubClient{
		aErr:      errors.New("upstream servfail boom"),
		aaaaDelay: 2 * time.Second,
		aaaaErr:   errors.New("too late"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ips, err := r.LookupIP(ctx, "example.com")
	if err == nil {
		t.Fatalf("LookupIP swallowed the A-query error: got ips=%v, err=nil", ips)
	}
	if !errors.Is(err, resolver.ErrIPNotFound) {
		t.Fatalf("error no longer satisfies ErrIPNotFound (tunnel.shouldStopRetry depends on it): %v", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("the real upstream cause was dropped: %v", err)
	}
}

// A 成功时，AAAA 失败不能影响结果。
func TestLookupIPKeepsIPv4WhenIPv6Fails(t *testing.T) {
	r := NewResolverFromClient(&stubClient{
		aIP:     "1.2.3.4",
		aaaaErr: errors.New("no aaaa"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ips, err := r.LookupIP(ctx, "example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ips) != 1 || ips[0].String() != "1.2.3.4" {
		t.Fatalf("got %v, want [1.2.3.4]", ips)
	}
}

// 两条腿都成功时，v4 在前、v6 也要带上。
func TestLookupIPMergesBothFamilies(t *testing.T) {
	r := NewResolverFromClient(&stubClient{
		aIP:    "1.2.3.4",
		aaaaIP: "2606:4700:4700::1111",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ips, err := r.LookupIP(ctx, "example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var hasV4, hasV6 bool
	for _, ip := range ips {
		if ip.Is4() {
			hasV4 = true
		}
		if ip.Is6() && !ip.Is4In6() {
			hasV6 = true
		}
	}
	if !hasV4 || !hasV6 {
		t.Fatalf("expected both families, got %v", ips)
	}
}
