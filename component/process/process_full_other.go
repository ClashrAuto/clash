//go:build !linux

package process

import "net/netip"

// 非 Linux 平台还没有「给全四元组就更便宜」的查法（darwin 是 sysctl 整块 pcblist、
// windows 是 GetExtendedTcpTable 整表），多给的对端地址用不上，原样转给既有实现。
func findProcessNameFull(network string, srcIP netip.Addr, srcPort int, _ netip.Addr, _ int) (uint32, string, error) {
	return findProcessName(network, srcIP, srcPort)
}
