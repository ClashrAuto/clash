package generator

import (
	"encoding/base64"
	"fmt"

	"github.com/ClashrAuto/coast/component/ech"
	"github.com/ClashrAuto/coast/transport/sudoku"
	"github.com/ClashrAuto/coast/transport/vless/encryption"

	"github.com/ClashrAuto/tide"
	"github.com/gofrs/uuid/v5"
)

func Main(args []string) {
	if len(args) < 1 {
		panic(usage)
	}
	switch args[0] {
	case "uuid":
		newUUID, err := uuid.NewV4()
		if err != nil {
			panic(err)
		}
		fmt.Println(newUUID.String())
	case "reality-keypair":
		privateKey, err := GenX25519PrivateKey()
		if err != nil {
			panic(err)
		}
		fmt.Println("PrivateKey: " + base64.RawURLEncoding.EncodeToString(privateKey.Bytes()))
		fmt.Println("PublicKey: " + base64.RawURLEncoding.EncodeToString(privateKey.PublicKey().Bytes()))
	case "wg-keypair":
		privateKey, err := GenX25519PrivateKey()
		if err != nil {
			panic(err)
		}
		fmt.Println("PrivateKey: " + base64.StdEncoding.EncodeToString(privateKey.Bytes()))
		fmt.Println("PublicKey: " + base64.StdEncoding.EncodeToString(privateKey.PublicKey().Bytes()))
	case "ech-keypair":
		if len(args) < 2 {
			panic("Using: generate ech-keypair <plain_server_name>")
		}
		configBase64, keyPem, err := ech.GenECHConfig(args[1])
		if err != nil {
			panic(err)
		}
		fmt.Println("Config:", configBase64)
		fmt.Println("Key:", keyPem)
	case "vless-mlkem768":
		var seed string
		if len(args) > 1 {
			seed = args[1]
		}
		seedBase64, clientBase64, hash32Base64, err := encryption.GenMLKEM768(seed)
		if err != nil {
			panic(err)
		}
		fmt.Println("Seed: " + seedBase64)
		fmt.Println("Client: " + clientBase64)
		fmt.Println("Hash32: " + hash32Base64)
		fmt.Println("-----------------------")
		fmt.Println("      Lazy-Config      ")
		fmt.Println("-----------------------")
		fmt.Printf("[Server] decryption: \"mlkem768x25519plus.native.600s.%s\"\n", seedBase64)
		fmt.Printf("[Client] encryption: \"mlkem768x25519plus.native.0rtt.%s\"\n", clientBase64)
	case "vless-x25519":
		var privateKey string
		if len(args) > 1 {
			privateKey = args[1]
		}
		privateKeyBase64, passwordBase64, hash32Base64, err := encryption.GenX25519(privateKey)
		if err != nil {
			panic(err)
		}
		fmt.Println("PrivateKey: " + privateKeyBase64)
		fmt.Println("Password: " + passwordBase64)
		fmt.Println("Hash32: " + hash32Base64)
		fmt.Println("-----------------------")
		fmt.Println("      Lazy-Config      ")
		fmt.Println("-----------------------")
		fmt.Printf("[Server] decryption: \"mlkem768x25519plus.native.600s.%s\"\n", privateKeyBase64)
		fmt.Printf("[Client] encryption: \"mlkem768x25519plus.native.0rtt.%s\"\n", passwordBase64)
	case "sudoku-keypair":
		privateKey, publicKey, err := sudoku.GenKeyPair()
		if err != nil {
			panic(err)
		}
		// Output: Available Private Key for client, Master Public Key for server
		fmt.Println("PrivateKey: " + privateKey)
		fmt.Println("PublicKey: " + publicKey)
	case "tide-keypair":
		// TIDE 服务端静态密钥。私钥进 listeners 的 private-key，
		// 公钥进客户端 proxies 的 public-key。
		//
		// ★ 这个子命令存在的理由是 Coast 桌面端是 C++：私钥是 X25519(32B) 与
		// ML-KEM-768 种子(64B) 的拼接，公钥是 X25519 公钥(32B) 与 ML-KEM-768
		// 封装公钥(1184B) 的拼接——后者在 C++ 侧没有可依赖的实现，而核心本来
		// 就链着 tide。让唯一持有该实现的进程来生成，比在两条产品线上各写一遍
		// 后量子密钥派生要少一整类出错方式。
		//
		// ⚠️ 私钥换掉等于所有客户端配置一起作废（public-key 是从它派生的），
		// 所以调用方必须持久化它，不能每次启动重新生成。
		k, err := tide.GenerateKey()
		if err != nil {
			panic(err)
		}
		fmt.Println("PrivateKey: " + k.String())
		fmt.Println("PublicKey: " + k.Public().String())
	default:
		// ★ 从前没有这个分支：子命令名打错就**静默退出 0**，什么都不输出。
		// 调用方（脚本、Coast 桌面端）拿到的是「成功但没有密钥」，
		// 而那会一路错到「配置生成了、核心起不来」才暴露。
		panic("unknown subcommand " + args[0] + "\n" + usage)
	}
}

const usage = "Using: generate uuid/reality-keypair/wg-keypair/ech-keypair/" +
	"vless-mlkem768/vless-x25519/sudoku-keypair/tide-keypair"
