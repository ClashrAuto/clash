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

// 真正去枚举网卡。做成变量是为了给测试一个缝隙 —— 否则「一波并发未命中会枚举几次」
// 这件事没法量化，而它正是下面 refreshMu 要解决的问题。
var enumerate = func() map[netip.Addr]struct{} {
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
	return set
}

// ★ 刷新必须**串行**，否则上面那句「不能让一波查不到的连接把刷新打成风暴」是空话：
//   间隔检查在 isLocalAddr 里、锁外做，而时间戳要等枚举完才更新 —— 3 秒窗口一到，
//   同时越过检查的每一个 goroutine 都会**各自完整枚举一遍网卡**。而「每条连接都未命中」
//   正是这个文件存在的场景（透明网关，源地址全是局域网别的机器）。
//   实测：64 条并发未命中会触发 64 次枚举；加上这把锁 + 双检之后是 1 次。
var refreshMu sync.Mutex

// seen 是调用方读到的那个时间戳。等锁期间若已经有人刷过（时间戳变了），直接返回 ——
// 那份结果对本次调用同样有效，没必要再枚举一遍。
func refreshLocalAddrs(seen time.Time) {
	refreshMu.Lock()
	defer refreshMu.Unlock()

	localAddrs.mu.RLock()
	cur := localAddrs.refresh
	localAddrs.mu.RUnlock()
	if cur.After(seen) {
		return
	}

	set := enumerate()
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
		refreshLocalAddrs(refreshed)
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
