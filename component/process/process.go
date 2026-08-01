package process

import (
	"errors"
	"net/netip"

	C "github.com/ClashrAuto/coast/constant"
)

var (
	ErrInvalidNetwork     = errors.New("invalid network")
	ErrPlatformNotSupport = errors.New("not support on this platform")
	ErrNotFound           = errors.New("process not found")
)

const (
	TCP = "tcp"
	UDP = "udp"
)

func FindProcessName(network string, srcIP netip.Addr, srcPort int) (uint32, string, error) {
	return FindProcessNameFull(network, srcIP, srcPort, netip.Addr{}, 0)
}

// FindProcessNameFull 与 FindProcessName 相同，但额外接收连接的**对端**地址。
//
// 多给这一半四元组是为了让 Linux 侧走 `inet_diag` 的**精确查询**而不是 `NLM_F_DUMP`：
// dump 要内核遍历整张 ehash 表（桶数按内存定，几十万个），耗时与本机实际 socket 数**无关**，
// 真机实测恒定 5.3ms/次；精确查询只做一次哈希查找，同机同条连接实测 **0.022ms**，
// 相差两个数量级。这项开销是**按连接**付的，直接压着代理的新建连接速率。
// 拿不到对端（传零值）时自动回退 dump，行为与从前一致。
func FindProcessNameFull(network string, srcIP netip.Addr, srcPort int, dstIP netip.Addr, dstPort int) (uint32, string, error) {
	// 源地址不是本机的任何一个地址 ⇒ 本机套接字表里不可能有它 ⇒ 查了也必然 ErrNotFound。
	// 各平台的实现都要把整张套接字表拉一遍（Linux 的 netlink dump / macOS 的
	// sysctl pcblist_n），做透明网关时绝大多数连接都是这一类，白烧的就是这一遍。
	// 详见 local_addr.go 的说明。
	if !isLocalAddr(srcIP) {
		return 0, "", ErrNotFound
	}
	return findProcessNameFull(network, srcIP, srcPort, dstIP, dstPort)
}

// PackageNameResolver
// never change type traits because it's used in CMFA
type PackageNameResolver func(metadata *C.Metadata) (string, error)

// DefaultPackageNameResolver
// never change type traits because it's used in CMFA
var DefaultPackageNameResolver PackageNameResolver

func FindPackageName(metadata *C.Metadata) (string, error) {
	if resolver := DefaultPackageNameResolver; resolver != nil {
		return resolver(metadata)
	}
	return "", ErrPlatformNotSupport
}
