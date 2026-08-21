package generator

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"time"

	"github.com/ClashrAuto/coast/component/ca"
)

// GenSelfSignedCert 生成一张自签证书及其私钥（都是 PEM），外加 SHA256 指纹。
//
// ★ 为什么不直接用 component/ca 的 NewRandomTLSKeyPair：那个函数的 template 里
// 只有序列号和有效期——**没有 CN，没有 SAN**。用它去撑一个以伪装为设计目标的
// 入站（TIDE）是自相矛盾的：一台在 443 上服务 HTTPS、证书却连主机名都不填的
// 机器，探测方看一眼就知道不是真站点。而且没有 SAN 就永远只能配
// skip-cert-verify: true，把「将来换成真证书」这条路也堵死了。
//
// 也不去改 component/ca：那是上游 mihomo 的文件，动它会在每次 merge 上游时
// 多一处冲突面（见 CLAUDE.md 对 fork 的约定）。generator 包本来就有自有文件
// （x25519.go），新增放这里合并面为零。
//
// host 同时写进 CN 与 SAN；是 IP 字面量就进 IPAddresses，否则进 DNSNames。
func GenSelfSignedCert(host string, keyPairType ca.KeyPairType) (certPEM, keyPEM, fingerprint string, err error) {
	var key crypto.Signer
	switch keyPairType {
	case ca.KeyPairTypeRSA:
		key, err = rsa.GenerateKey(rand.Reader, 2048)
	case ca.KeyPairTypeP384:
		key, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	default: // P256：比 RSA 快得多、握手字节也少，默认就用它
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	if err != nil {
		return
	}

	// 序列号必须随机。固定值（component/ca 那边写死 1）会让同一台机器上
	// 先后生成的两张证书拥有相同序列号，某些校验链路会直接判为重放。
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return
	}

	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		// 回退一小时而不是一年：证书越"新鲜"越像真站点按期轮换的产物。
		// 但也不能从此刻起算——客户端与服务端的钟差会让刚签发的证书
		// 在对面看来"尚未生效"，那是一条极难归因的握手失败。
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, key.Public(), key)
	if err != nil {
		return
	}
	privBytes, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return
	}
	fingerprint = ca.CalculateFingerprint(certDER)
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes}))
	return
}
