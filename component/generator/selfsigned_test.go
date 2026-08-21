package generator

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"github.com/ClashrAuto/coast/component/ca"
)

func parseCert(t *testing.T, certPEM string) *x509.Certificate {
	t.Helper()
	blk, _ := pem.Decode([]byte(certPEM))
	if blk == nil {
		t.Fatal("证书不是合法 PEM")
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatalf("解析证书: %v", err)
	}
	return c
}

// TestSelfSignedCertLoadableByListener 钉住真正的集成契约：
// 我们生成的这对 PEM，必须是 listener/tide 的 loadKeyPair 收得下的东西
// （它走的就是 tls.X509KeyPair）。密钥类型或 PEM 头写错都没有编译错误，
// 只在核心加载配置时才炸。
func TestSelfSignedCertLoadableByListener(t *testing.T) {
	for _, kt := range []ca.KeyPairType{ca.KeyPairTypeP256, ca.KeyPairTypeP384, ca.KeyPairTypeRSA} {
		certPEM, keyPEM, fp, err := GenSelfSignedCert("coast.local", kt)
		if err != nil {
			t.Fatalf("%s: %v", kt, err)
		}
		if _, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM)); err != nil {
			t.Fatalf("%s: listener 侧加载不了这对 PEM: %v", kt, err)
		}
		if fp == "" {
			t.Fatalf("%s: 指纹为空", kt)
		}
	}
}

// TestSelfSignedCertHostGoesToRightField —— 主机名进 DNSNames 还是 IPAddresses
// 是按字面量判的。搞反了 TLS 校验永远过不了，而默认配置是 skip-cert-verify，
// 于是这个错误会一直藏到有人想换真证书那天。
func TestSelfSignedCertHostGoesToRightField(t *testing.T) {
	cases := []struct {
		host  string
		isIP  bool
		label string
	}{
		{"coast.local", false, "域名"},
		{"192.168.31.71", true, "IPv4"},
		// 方案 4 的客户端连的就是 IPv6 字面量，这一条是主路径不是边角。
		{"240e:3a3:7e41:5310:1852:7281:b8cb:7d5b", true, "IPv6"},
	}
	for _, c := range cases {
		certPEM, _, _, err := GenSelfSignedCert(c.host, ca.KeyPairTypeP256)
		if err != nil {
			t.Fatalf("%s: %v", c.label, err)
		}
		cert := parseCert(t, certPEM)
		if cert.Subject.CommonName != c.host {
			t.Fatalf("%s: CN = %q，期望 %q", c.label, cert.Subject.CommonName, c.host)
		}
		if c.isIP {
			if len(cert.IPAddresses) != 1 || len(cert.DNSNames) != 0 {
				t.Fatalf("%s: 期望进 IPAddresses，实得 IP=%v DNS=%v",
					c.label, cert.IPAddresses, cert.DNSNames)
			}
		} else {
			if len(cert.DNSNames) != 1 || len(cert.IPAddresses) != 0 {
				t.Fatalf("%s: 期望进 DNSNames，实得 IP=%v DNS=%v",
					c.label, cert.IPAddresses, cert.DNSNames)
			}
		}
	}
}

// TestSelfSignedCertNotBeforeIsInThePast —— 两端钟差会让「此刻起算」的证书
// 在对面看来尚未生效，那是一条极难归因的握手失败。
func TestSelfSignedCertNotBeforeIsInThePast(t *testing.T) {
	certPEM, _, _, err := GenSelfSignedCert("coast.local", ca.KeyPairTypeP256)
	if err != nil {
		t.Fatal(err)
	}
	cert := parseCert(t, certPEM)
	if !cert.NotBefore.Before(time.Now().Add(-30 * time.Minute)) {
		t.Fatalf("NotBefore = %v，没有留出足够的钟差余量", cert.NotBefore)
	}
	if !cert.NotAfter.After(time.Now().AddDate(0, 11, 0)) {
		t.Fatalf("NotAfter = %v，有效期短于预期的一年", cert.NotAfter)
	}
}

// TestSelfSignedCertSerialIsRandom —— component/ca 那份把序列号写死成 1，
// 于是同一台机器先后生成的两张证书序列号相同，某些校验链路会判成重放。
func TestSelfSignedCertSerialIsRandom(t *testing.T) {
	a, _, _, err := GenSelfSignedCert("coast.local", ca.KeyPairTypeP256)
	if err != nil {
		t.Fatal(err)
	}
	b, _, _, err := GenSelfSignedCert("coast.local", ca.KeyPairTypeP256)
	if err != nil {
		t.Fatal(err)
	}
	if parseCert(t, a).SerialNumber.Cmp(parseCert(t, b).SerialNumber) == 0 {
		t.Fatal("两次生成拿到同一个序列号")
	}
}
