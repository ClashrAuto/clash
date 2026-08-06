package inbound

import (
	"strings"

	C "github.com/ClashrAuto/coast/constant"
	LC "github.com/ClashrAuto/coast/listener/config"
	"github.com/ClashrAuto/coast/listener/tide"
	"github.com/ClashrAuto/coast/log"
)

type TideOption struct {
	BaseOption
	Users map[string]string `inbound:"users,omitempty"`
	// PrivateKey 是 TIDE 的静态密钥（base64，96 字节种子），与 TLS 私钥是两回事。
	PrivateKey string `inbound:"private-key"`
	// Certificate / PrivateKeyPEM 是外层 TLS 的证书，PEM 内容或文件路径都接受。
	Certificate   string `inbound:"certificate"`
	PrivateKeyPEM string `inbound:"private-key-pem"`
	// Cover 是掩护源站，必填。见 listener/config/tide.go 的说明。
	Cover      string `inbound:"cover"`
	QUICListen string `inbound:"quic-listen,omitempty"`
	AllowBare  bool   `inbound:"allow-bare,omitempty"`
	Congestion string `inbound:"congestion,omitempty"`
}

func (o TideOption) Equal(config C.InboundConfig) bool {
	return optionToString(o) == optionToString(config)
}

type Tide struct {
	*Base
	config *TideOption
	l      C.MultiAddrListener
	vs     LC.TideServer
}

func NewTide(options *TideOption) (*Tide, error) {
	base, err := NewBase(&options.BaseOption)
	if err != nil {
		return nil, err
	}
	return &Tide{
		Base:   base,
		config: options,
		vs: LC.TideServer{
			Enable:        true,
			Listen:        base.RawAddress(),
			QUICListen:    options.QUICListen,
			Users:         options.Users,
			PrivateKey:    options.PrivateKey,
			Certificate:   options.Certificate,
			PrivateKeyPEM: options.PrivateKeyPEM,
			Cover:         options.Cover,
			AllowBare:     options.AllowBare,
			Congestion:    options.Congestion,
		},
	}, nil
}

// Config implements constant.InboundListener
func (v *Tide) Config() C.InboundConfig { return v.config }

// Address implements constant.InboundListener
func (v *Tide) Address() string {
	var addrList []string
	if v.l != nil {
		for _, addr := range v.l.AddrList() {
			addrList = append(addrList, addr.String())
		}
	}
	return strings.Join(addrList, ",")
}

// Listen implements constant.InboundListener
func (v *Tide) Listen(tunnel C.Tunnel) error {
	var err error
	v.l, err = tide.New(v.vs, v.ListenConfig(), tunnel, v.Additions()...)
	if err != nil {
		return err
	}
	log.Infoln("Tide[%s] proxy listening at: %s", v.Name(), v.Address())
	return nil
}

// Close implements constant.InboundListener
func (v *Tide) Close() error { return v.l.Close() }

var _ C.InboundListener = (*Tide)(nil)
