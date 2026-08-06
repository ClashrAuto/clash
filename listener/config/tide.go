package config

import (
	"encoding/json"
)

type TideServer struct {
	Enable bool   `yaml:"enable" json:"enable"`
	Listen string `yaml:"listen" json:"listen"`
	// QUICListen 是可选的加速通道。TIDE 的抗主动探测完全靠 TCP 那条路径
	// （QUIC 路径不做掩护转发，见 tide spec §12.6），所以它绝不能单独开放。
	QUICListen string            `yaml:"quic-listen,omitempty" json:"quic-listen,omitempty"`
	Users      map[string]string `yaml:"users" json:"users,omitempty"`
	// PrivateKey 是 TIDE 的静态密钥（base64，96 字节种子），
	// 用 `tide-selftest -mode keygen` 生成。注意它和 TLS 的私钥是两回事。
	PrivateKey string `yaml:"private-key" json:"private-key,omitempty"`
	// Certificate / PrivateKeyPEM 是外层 TLS 的证书，PEM 内容或文件路径都接受。
	Certificate   string `yaml:"certificate" json:"certificate,omitempty"`
	PrivateKeyPEM string `yaml:"private-key-pem" json:"private-key-pem,omitempty"`
	// Cover 是掩护源站 host:port。**必填**：认证失败时这条连接的全部字节会被原样
	// 转发过去，时序是伪装里唯一真正难伪造的东西。填 "drop" 表示明知风险仍放弃伪装。
	Cover string `yaml:"cover" json:"cover,omitempty"`
	// AllowBare 允许协商裸帧模式（内层不加密，安全性完全由外层 TLS 承担）。
	AllowBare bool `yaml:"allow-bare,omitempty" json:"allow-bare,omitempty"`
}

func (t TideServer) String() string {
	b, _ := json.Marshal(t)
	return string(b)
}
