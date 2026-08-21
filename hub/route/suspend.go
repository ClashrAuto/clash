package route

import (
	"github.com/ClashrAuto/coast/component/suspend"

	"github.com/metacubex/chi"
	"github.com/metacubex/chi/render"
	"github.com/metacubex/http"
)

// Coast 自有：/suspend —— 「设备睡了，周期性后台活动也睡」（健康检查、statistic 归零）。
//
// ★ Android 的核心是子进程，REST 是**唯一**的运行时通道，所以这个开关必须在这儿有个门；
//
//	iOS 隧道进程走 C 接口（libcoast 的 CoastSuspend/CoastResume），不经这里。
//
// ★ 语义与取舍全写在 `component/suspend` 包顶 —— 别在这层复述，会漂。
// ★ 老客户端打到旧核心时这里是 404 —— 客户端必须把 404 当「核心不支持，静默作罢」，
//
//	不能当错误弹给用户（那会把一次正常的版本组合变成一条天天弹的假故障）。
func suspendRouter() http.Handler {
	r := chi.NewRouter()
	r.Get("/", getSuspend)
	r.Put("/", setSuspend)
	return r
}

func getSuspend(w http.ResponseWriter, r *http.Request) {
	render.JSON(w, r, render.M{"suspended": suspend.Suspended()})
}

func setSuspend(w http.ResponseWriter, r *http.Request) {
	req := struct {
		Suspended bool `json:"suspended"`
	}{}
	if err := render.DecodeJSON(r.Body, &req); err != nil {
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, ErrBadRequest)
		return
	}
	if req.Suspended {
		suspend.Suspend()
	} else {
		suspend.Resume()
	}
	render.NoContent(w, r)
}
