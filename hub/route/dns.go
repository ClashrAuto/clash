package route

import (
	"context"
	"math"

	"github.com/ClashrAuto/coast/component/resolver"
	"github.com/ClashrAuto/coast/log"

	"github.com/metacubex/chi"
	"github.com/metacubex/chi/render"
	"github.com/metacubex/http"
	"github.com/miekg/dns"
	"github.com/samber/lo"
)

func dnsRouter() http.Handler {
	r := chi.NewRouter()
	r.Get("/query", queryDNS)
	r.Post("/reset", resetDNS)
	return r
}

// Coast 自有：POST /dns/reset —— 重置 DNS 上游解析连接（DoH/DoT 长连接）。
//
// ★ 与 libcoast 的 CoastResetDNS 同一个原语：治「长睡醒来 DoH 假活,国内首访
//   每条等内核 shouldRetry 超时」。子进程形态（Qt 桌面 / Android）此前唯一的
//   通道是整份 `PUT /configs`（ApplyConfig 的开销全是搭进去的）——现在有了
//   轻原语:不重载配置、不重建规则树、不触发健康检查,立刻返回。
// ★ 老客户端打到旧核心时这里是 404 —— 客户端必须把 404 当「核心不支持,
//   静默作罢」,与 /suspend 同一条纪律。
func resetDNS(w http.ResponseWriter, r *http.Request) {
	log.Infoln("REST: reset DNS upstream connections (wake recovery)")
	resolver.ResetConnection()
	render.NoContent(w, r)
}

func queryDNS(w http.ResponseWriter, r *http.Request) {
	if resolver.DefaultResolver == nil {
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, newError("DNS section is disabled"))
		return
	}

	name := r.URL.Query().Get("name")
	qTypeStr, _ := lo.Coalesce(r.URL.Query().Get("type"), "A")

	qType, exist := dns.StringToType[qTypeStr]
	if !exist {
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, newError("invalid query type"))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), resolver.DefaultDNSTimeout)
	defer cancel()

	msg := dns.Msg{}
	msg.SetQuestion(dns.Fqdn(name), qType)
	resp, err := resolver.DefaultResolver.ExchangeContext(ctx, &msg)
	if err != nil {
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, newError(err.Error()))
		return
	}

	responseData := render.M{
		"Status":   resp.Rcode,
		"Question": resp.Question,
		"TC":       resp.Truncated,
		"RD":       resp.RecursionDesired,
		"RA":       resp.RecursionAvailable,
		"AD":       resp.AuthenticatedData,
		"CD":       resp.CheckingDisabled,
	}

	rr2Json := func(rr dns.RR, _ int) render.M {
		header := rr.Header()
		return render.M{
			"name": header.Name,
			"type": header.Rrtype,
			"TTL":  header.Ttl,
			"data": lo.Substring(rr.String(), len(header.String()), math.MaxUint),
		}
	}

	if len(resp.Answer) > 0 {
		responseData["Answer"] = lo.Map(resp.Answer, rr2Json)
	}
	if len(resp.Ns) > 0 {
		responseData["Authority"] = lo.Map(resp.Ns, rr2Json)
	}
	if len(resp.Extra) > 0 {
		responseData["Additional"] = lo.Map(resp.Extra, rr2Json)
	}

	render.JSON(w, r, responseData)
}
