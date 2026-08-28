package listener

import (
	"testing"

	LC "github.com/ClashrAuto/coast/listener/config"
	"github.com/ClashrAuto/coast/tunnel"
)

// 一份「上一轮隧道会话」形状的 tun 配置。fd 故意给一个必然无效的号
// （超出 rlimit，dup/fcntl 一律 EBADF）：测试不需要真的建出 tun，
// 只需要区分「走到了创建路径」与「被 Equal(LastTunConf) 跳过」。
func restartShapedTunConf() LC.Tun {
	return LC.Tun{
		Enable:    true,
		DNSHijack: []string{"198.18.0.2:53"},
		AutoRoute: false,
		// 不开 auto-detect-interface：它会真的拉起 sing-tun 的接口监视器，
		// 而测试只需要「两轮配置逐字节相同」这一个性质。
		AutoDetectInterface: false,
		MTU:                 1500,
		FileDescriptor:      1 << 20,
	}
}

// Cleanup 之后 LastTunConf 必须清零。
//
// 这个包的生命周期不等于核心的生命周期：macOS 系统扩展 / iOS NE 里核心是
// 同进程内 stop→start 的（CoastStop/CoastStart），包级变量原样活过重启。
// 只关监听器不清 LastTunConf 的话，GetTunConf 会把上一轮的配置当成现状报出去。
func TestCleanupForgetsLastTunConf(t *testing.T) {
	conf := restartShapedTunConf()
	tunMux.Lock()
	LastTunConf = conf
	tunMux.Unlock()

	Cleanup()

	if got := GetTunConf(); got.Enable {
		t.Fatalf("Cleanup 之后 GetTunConf().Enable 仍是 true：LastTunConf 没被清零，"+
			"同进程重启后同号 fd 的配置会被判「没变」而跳过创建（got=%+v）", got)
	}
}

// 同进程重启的完整形状：上一轮的配置还躺在 LastTunConf 里 → Cleanup（停隧道）
// → 用逐字节相同的配置再来一轮 ReCreateTun（起隧道，fd 号恰好复用）。
//
// 修复前的行为：Equal(LastTunConf) 判「没变」直接返回，而 tunLister 已是 nil ——
// TUN 永远不会被创建，且成功行与报错行一行都没有。表现是 VPN Connected、
// utun 存在、路由正常、混合端口能用，唯独走隧道的流量全部超时
// （2026-08-28 真机：fd 14→14 黑洞，重启拿到 41 恢复；08-26 的 35→35 同因）。
//
// 判据：走到创建路径的证据是失败 defer 把 Enable 翻成 false 存进 LastTunConf
// （fd 无效，sing_tun.New 必然失败）；被跳过的话 Enable 保持 true。
func TestReCreateTunAfterCleanupAttemptsCreation(t *testing.T) {
	conf := restartShapedTunConf()
	tunMux.Lock()
	LastTunConf = conf
	tunMux.Unlock()

	Cleanup()
	// 第二个参数用真的 tunnel.Tunnel（executor 传的就是它）：
	// sing_tun.New 一进门就 tunnel.(P.Tunnel)，nil 接口会 panic 而不是报错。
	ReCreateTun(restartShapedTunConf(), tunnel.Tunnel)

	tunMux.Lock()
	got := LastTunConf
	tunMux.Unlock()
	if got.Enable {
		t.Fatal("重启后的 ReCreateTun 被 Equal(LastTunConf) 跳过了：" +
			"创建路径根本没跑（跑了的话无效 fd 会让失败路径把 Enable 翻成 false）")
	}
}
