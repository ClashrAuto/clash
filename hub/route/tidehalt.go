package route

import (
	"encoding/json"

	"github.com/ClashrAuto/coast/adapter/outbound"

	"github.com/metacubex/chi"
	"github.com/metacubex/chi/render"
	"github.com/metacubex/http"
)

// Coast 自有：/tidehalt —— TIDE 出站 halt 档的「已停止重试」事件流。
//
// 「我的电脑」卡片上把「连不上时」设成「不使用」时，节点带 `halt-on-fail: true`；
// 连败熔断进入 halt 档（adapter/outbound/tide_breaker.go）后投一条事件，
// **平台侧要把它发成系统通知** —— 这个状态成立的典型时刻是深夜，通知是唯一
// 能到达用户的通道。
//
// ★ Android 的核心是子进程，REST 是唯一的运行时通道：app 的服务挂一条**常驻的
// 流式 GET** 在这里等事件（回环上的空闲 TCP，零流量零唤醒——比轮询省电，也即时）。
// iOS 核心在隧道进程内，走 libcoast 的 CoastNextTideHalt 轮询，不经这里。
//
// ★ 老客户端打到旧核心时这里是 404 —— 客户端必须把 404/断开当「核心不支持，
// 静默作罢」，不能当错误弹给用户（与 /suspend 同一条纪律）。
//
// 响应体：每条事件一行 JSON `{"name":"<出站名>"}`，随事件即时 flush；
// 连接一直挂着直到客户端断开。积压在通道里的事件（有界缓冲 8 条）连上即发。
func tideHaltRouter() http.Handler {
	r := chi.NewRouter()
	r.Get("/", getTideHalts)
	return r
}

func getTideHalts(w http.ResponseWriter, r *http.Request) {
	streamTideHalts(w, r, outbound.TideHaltEventsChan())
}

// streamTideHalts 拆出来是为了可测：通道由调用方注入。
func streamTideHalts(w http.ResponseWriter, r *http.Request, ch <-chan string) {
	w.Header().Set("Content-Type", "application/json")
	render.Status(r, http.StatusOK)
	// 先把头 flush 出去：客户端要立刻知道「连上了、核心支持这个端点」，
	// 而不是等第一条事件（可能几天都没有）。
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	for {
		select {
		case name := <-ch:
			buf, err := json.Marshal(map[string]string{"name": name})
			if err != nil {
				return
			}
			if _, err := w.Write(append(buf, '\n')); err != nil {
				// 客户端断开：事件已从通道里取走，就地丢弃 ——
				// halt 通知是尽力而为，绝不能反过来卡住谁。
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		case <-r.Context().Done():
			return
		}
	}
}
