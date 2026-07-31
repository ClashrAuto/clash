package config

import "github.com/ClashrAuto/coast/transport/kcptun"

type KcpTun struct {
	Enable        bool `json:"enable"`
	kcptun.Config `json:",inline"`
}
