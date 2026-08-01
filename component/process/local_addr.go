package process

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

// 本机自有地址的缓存 —— 给 FindProcessName 做「这条连接可能属于本机进程吗」的前置判断。
//
// **为什么需要它**：查进程的实现在每个平台上都是「把整张套接字表 dump 出来再线性找」——
// Linux 是一次 NETLINK_INET_DIAG 的 NLM_F_DUMP（连 TIME_WAIT 一起拉），macOS 是
// `sysctl net.inet.tcp.pcblist_n` 整块拷贝，然后还要再扫一遍 /proc/<pid>/fd 找 inode。
// 这是**按连接**付费的，且与本机套接字总数成正比。
//
// 而当本进程在做透明网关/旁路由时，绝大多数连接的源地址是**局域网里别的机器**。
// 那些连接在本机的套接字表里根本不存在，dump 完必然是 ErrNotFound —— 白烧一遍。
// 真机压测（树莓派当网关、被代理设备打满并发）实测：find-process-mode=always 时
// 整体只有 207~309 conn/s，而关掉进程查找是 5226 conn/s，**17 倍**。
//
// 这里做的不是「不查」，而是**只跳过必然查不到的那一类**：源地址若不是本机任何一个
// 接口上的地址，本机就不可能有对应的套接字，跳过与 dump 一遍再返回 not found 完全等价。
// 本机自己发起的连接（源地址就是本机地址或回环）行为一字不变。
var localAddrs = struct {
	mu      sync.RWMutex
	set     map[netip.Addr]struct{}
	refresh time.Time
}{}

// 缓存的最短刷新间隔。地址变动（连上 Wi-Fi、TUN 起来、DHCP 换地址）要能被看到，
// 但也不能让一波查不到的连接把刷新打成风暴 —— 所以「未命中才刷新，且至少隔这么久」。
const localAddrRefreshInterval = 3 * time.Second

func refreshLocalAddrs() {
	set := make(map[netip.Addr]struct{}, 16)
	if ifaces, err := net.Interfaces(); err == nil {
		for _, iface := range ifaces {
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				var ip net.IP
				switch v := addr.(type) {
				case *net.IPNet:
					ip = v.IP
				case *net.IPAddr:
					ip = v.IP
				default:
					continue
				}
				if a, ok := netip.AddrFromSlice(ip); ok {
					set[a.Unmap()] = struct{}{}
				}
			}
		}
	}
	localAddrs.mu.Lock()
	// 拿不到任何地址时**不要**用空集合覆盖：那会把所有连接都判成「非本机」，
	// 等于悄悄关掉进程查找。宁可继续用上一份（顶多多 dump 几次）。
	if len(set) > 0 {
		localAddrs.set = set
	}
	localAddrs.refresh = time.Now()
	localAddrs.mu.Unlock()
}

// isLocalAddr 报告 ip 是不是本机自己的地址（回环与未指定地址一律算）。
func isLocalAddr(ip netip.Addr) bool {
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsLoopback() {
		return true
	}
	ip = ip.Unmap()

	localAddrs.mu.RLock()
	set, refreshed := localAddrs.set, localAddrs.refresh
	localAddrs.mu.RUnlock()

	if set != nil {
		if _, ok := set[ip]; ok {
			return true
		}
	}
	// 未命中：可能是缓存过期（刚拿到新地址），也可能真的是别人的地址。
	// 只在超过刷新间隔时才真去枚举网卡，然后再判一次；仍未命中才认定非本机。
	if set == nil || time.Since(refreshed) >= localAddrRefreshInterval {
		refreshLocalAddrs()
		localAddrs.mu.RLock()
		set = localAddrs.set
		localAddrs.mu.RUnlock()
		if set != nil {
			if _, ok := set[ip]; ok {
				return true
			}
		}
	}
	return false
}
