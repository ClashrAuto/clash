package generator

import (
	"strings"
	"testing"

	"github.com/ClashrAuto/tide"
)

// TestTideKeypairRoundTrip 钉住 `generate tide-keypair` 的**输出格式契约**。
//
// ★ 它测的不是 tide 自己的密钥学，而是「这个子命令打印的两个串，正好是
// 两侧消费端解析得动的那两个编码」：
//   - PrivateKey → listener/tide/server.go 的 tideproto.ParsePrivateKey
//   - PublicKey  → adapter/outbound/tide.go 的 tide.ParsePublicKey
//
// 选错编码（例如把私钥打成 Seed() 的原始字节、或用 StdEncoding 而不是
// RawURLEncoding）**没有任何编译错误**，配置也照样生成得出来，
// 只在核心真去加载那份配置时才炸——而那时现场已经离生成点很远了。
func TestTideKeypairRoundTrip(t *testing.T) {
	k, err := tide.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	privStr := k.String()
	pubStr := k.Public().String()

	if _, err := tide.ParsePrivateKey(privStr); err != nil {
		t.Fatalf("listener 侧解析不了我们打印的 PrivateKey: %v", err)
	}
	if _, err := tide.ParsePublicKey(pubStr); err != nil {
		t.Fatalf("outbound 侧解析不了我们打印的 PublicKey: %v", err)
	}

	// 尺寸是配对流程的设计约束，不是随便断言的：
	// 公钥 1216B → base64 约 1624 字符，塞不进一张好扫的二维码，
	// 所以配对必须走局域网交换而不是扫码。这条断言是那个决定的依据，
	// 哪天它变小了，配对方案可以回头简化。
	if got := len(k.Public().Bytes()); got != 1216 {
		t.Fatalf("公钥字节数 = %d，期望 1216（X25519 32 + ML-KEM-768 1184）", got)
	}
	if len(pubStr) < 1600 {
		t.Fatalf("公钥 base64 长度 = %d，短得意外——配对方案是按 ~1624 设计的", len(pubStr))
	}
}

// TestGeneratorUsageListsTideKeypair 防止加了子命令却忘了写进 usage：
// 那会让用户在 panic 信息里看不到它，等于没加。
func TestGeneratorUsageListsTideKeypair(t *testing.T) {
	if !strings.Contains(usage, "tide-keypair") {
		t.Fatalf("usage 串里没有 tide-keypair: %q", usage)
	}
}
