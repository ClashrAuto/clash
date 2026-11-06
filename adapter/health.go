package adapter

import (
	C "github.com/ClashrAuto/coast/constant"

	"github.com/ClashrAuto/coast/common/atomic"
	"github.com/ClashrAuto/coast/common/queue"
)

// Health 一个出站的测速结果（Coast 自有）。
//
// 重载配置时出站对象全部重建，测速结果留在旧对象上一起没了。周期测速的平台无所谓，下一轮
// 就补回来；iOS 没有周期测速（adapter/provider/healthcheck_ondemand.go），丢了就只能再全量
// 测一轮，而锁屏时节点是卸载的，每次解锁都要重载一次。所以按出站名把结果搬到新对象上。
type Health struct {
	alive   bool
	history []C.DelayHistory
	extra   map[string]urlHealth
}

type urlHealth struct {
	alive   bool
	history []C.DelayHistory
}

// Health 取出这个出站的测速结果。
func (p *Proxy) Health() Health {
	h := Health{
		alive:   p.alive.Load(),
		history: p.history.Copy(),
		extra:   map[string]urlHealth{},
	}
	p.extra.Range(func(url string, s *internalProxyState) bool {
		h.extra[url] = urlHealth{alive: s.alive.Load(), history: s.history.Copy()}
		return true
	})
	return h
}

// RestoreHealth 把测速结果放回这个出站。只该用在刚建好、还没人用过的对象上。
func (p *Proxy) RestoreHealth(h Health) {
	p.alive.Store(h.alive)
	p.history.Put(h.history...)
	for url, s := range h.extra {
		state := &internalProxyState{
			alive:   atomic.NewBool(s.alive),
			history: queue.New[C.DelayHistory](defaultHistoriesNum),
		}
		state.history.Put(s.history...)
		p.extra.Store(url, state)
	}
}
