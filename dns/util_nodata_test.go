package dns

import (
	"testing"

	D "github.com/miekg/dns"
)

// RFC 2308：NODATA 响应不带 SOA 就没法被下游否定缓存，
// dns.ipv6: false 下每次连接都会把同一个 AAAA 重问一遍。
func TestHandleMsgWithEmptyAnswerCarriesSOA(t *testing.T) {
	q := &D.Msg{}
	q.SetQuestion("example.com.", D.TypeAAAA)

	msg := handleMsgWithEmptyAnswer(q)

	if msg.Rcode != D.RcodeSuccess {
		t.Fatalf("rcode = %d, want NOERROR", msg.Rcode)
	}
	if len(msg.Answer) != 0 {
		t.Fatalf("answer section should stay empty, got %v", msg.Answer)
	}
	if len(msg.Ns) != 1 {
		t.Fatalf("authority section has %d records, want 1 SOA (否定缓存拿不到依据)", len(msg.Ns))
	}
	soa, ok := msg.Ns[0].(*D.SOA)
	if !ok {
		t.Fatalf("authority record is %T, want *dns.SOA", msg.Ns[0])
	}
	if soa.Hdr.Name != "example.com." {
		t.Fatalf("SOA owner = %q, want the query name", soa.Hdr.Name)
	}
	if soa.Hdr.Ttl == 0 || soa.Minttl == 0 {
		t.Fatalf("SOA ttl=%d minttl=%d, both must be non-zero or negative caching is a no-op",
			soa.Hdr.Ttl, soa.Minttl)
	}
	// 响应必须还能正常打包，否则客户端收到的是残包
	if _, err := msg.Pack(); err != nil {
		t.Fatalf("msg.Pack: %v", err)
	}
}

// 没有 question 的畸形请求不能 panic
func TestHandleMsgWithEmptyAnswerNoQuestion(t *testing.T) {
	msg := handleMsgWithEmptyAnswer(&D.Msg{})
	if len(msg.Ns) != 0 {
		t.Fatalf("no question, so no SOA should be synthesized, got %v", msg.Ns)
	}
}
