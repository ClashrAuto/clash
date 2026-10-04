package route

import (
	"github.com/ClashrAuto/coast/component/mmdb"

	"github.com/metacubex/chi"
	"github.com/metacubex/chi/render"
	"github.com/metacubex/http"
)

// Coast 自有：/geo/reload —— app 换上新的 Country.mmdb 之后叫核心改用它（见 mmdb.ReopenIP）。
//
// ★ 不复用上游的 POST /configs/geo：那条是核心**自己**去 geox-url 下载，下到的哈希与磁盘上
// 一致就直接返回、根本不重载 —— 而 app 刚换上的正是那份，于是永远不会生效。
func geoRouter() http.Handler {
	r := chi.NewRouter()
	r.Post("/reload", reloadGeo)
	return r
}

func reloadGeo(w http.ResponseWriter, r *http.Request) {
	render.JSON(w, r, render.M{"reopened": mmdb.ReopenIP()})
}
