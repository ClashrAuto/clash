package route

import (
	"strings"
	"testing"

	"github.com/metacubex/http/httptest"
)

// /geo/reload 要真的挂在鉴权组里（app 用它叫核心换上新的 Country.mmdb）。
func TestGeoReloadRoute(t *testing.T) {
	h := router(false, "s3cret", "", Cors{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/geo/reload", nil))
	if rec.Code != 401 {
		t.Fatalf("不带口令应被拒，得到 %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/geo/reload", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("POST /geo/reload 得到 %d: %s", rec.Code, rec.Body.String())
	}
	// 这个进程里没加载过 IP 库：没有东西可换，如实说 false。
	if body := strings.TrimSpace(rec.Body.String()); body != `{"reopened":false}` {
		t.Fatalf("回包 %q", body)
	}
}
