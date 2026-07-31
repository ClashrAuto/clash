package dns

// export functions from tunnel module

import "github.com/ClashrAuto/coast/tunnel"

const RespectRules = tunnel.DnsRespectRules

type dnsDialer = tunnel.DNSDialer

var newDNSDialer = tunnel.NewDNSDialer
