package dialer

import "context"

// —— 按入站决定出口网卡（per-inbound egress interface）——
//
// 出口网卡在本模块里一直是**全局量**（DefaultInterface / DefaultInterfaceFinder）。对普通客户端
// 没问题——一台机器只有一个"出网方向"。但对**透明网关**类用法不成立：一台机器同时接两条上行、
// 两边各有一批设备时，每台设备的正确出口是**它自己挂着的那条**，不是全局默认那条。
// 走错的代价是实打实的：设备流量被绕到另一条宽带上出去、延迟变高；而且全局默认还会随系统
// 跃点数变化（Wi-Fi 协商速率一变就换卡），在途连接成批断。
//
// 这件事在配置里表达不了：网卡只能绑在**出站对象**上（proxy 的 interface-name / dialer-proxy），
// 而"哪张卡"是**入站连接的属性**；两者要对上只能把 身份 × 出站 做笛卡尔积（每张卡复制一份
// 节点表与策略组，健康检查跟着翻倍）。所以把它放回该在的地方：**连接自己身上**。
//
// 链路：listener 的 interface-name → inbound.WithEgressInterface 盖进 Metadata →
//
//	tunnel 拨号前塞进 ctx → 这里取出来 → dialContext/listenPacket 按优先级采用。
//
// 优先级（在 dialer.go 里实现，插在既有那条链的正中间）：
//
//	出站显式 interface-name  >  入站携带的  >  DefaultInterface  >  DefaultInterfaceFinder
//
// 不设时行为与以前**逐字节相同**，对既有配置零影响。
type egressIfaceKey struct{}

// WithEgressInterface 把「这条连接该从哪张网卡出去」带进 ctx。
// name 为空时原样返回：不留空值，免得下游分不清"设过但为空"和"没设过"。
func WithEgressInterface(ctx context.Context, name string) context.Context {
	if name == "" {
		return ctx
	}
	return context.WithValue(ctx, egressIfaceKey{}, name)
}

// EgressInterfaceFromContext 取出上面塞进去的网卡名；没有则空串。
func EgressInterfaceFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(egressIfaceKey{}).(string); ok {
		return v
	}
	return ""
}
