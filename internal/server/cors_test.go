package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCORSPreflight 预检：OPTIONS + Origin + Access-Control-Request-Method 应 204
// 并带齐 CORS 头，且不进 mux（mux 对 OPTIONS 回 405，会让浏览器判定跨域失败）。
func TestCORSPreflight(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)

	req := httptest.NewRequest(http.MethodOptions, "/v1/images/edits", nil)
	req.Header.Set("Origin", "https://infinite-atelier.pages.dev")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("preflight code=%d want 204 (body=%s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://infinite-atelier.pages.dev" {
		t.Errorf("allow-origin=%q", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Headers"); got != "authorization,content-type" {
		t.Errorf("allow-headers=%q (应回显浏览器声明)", got)
	}
	if rr.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Error("allow-methods missing")
	}
	if rr.Header().Get("Access-Control-Max-Age") == "" {
		t.Error("max-age missing")
	}
	// 预检不得触达上游。
	if rec.imageBody != "" {
		t.Errorf("preflight must not reach upstream: %s", rec.imageBody)
	}
}

// TestCORSActualRequest 实际跨域请求（带 Origin 的 POST）：响应也必须回 CORS 头，
// 否则浏览器会丢弃响应体（axios 报 ERR_NETWORK）。
func TestCORSActualRequest(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)

	req := authedReq("POST", "/v1/images/generations", `{"model":"cn:hunyuan-image-alpha","prompt":"a cat"}`)
	req.Header.Set("Origin", "https://infinite-atelier.pages.dev")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://infinite-atelier.pages.dev" {
		t.Errorf("allow-origin=%q", got)
	}
	if !strings.Contains(rr.Header().Get("Vary"), "Origin") {
		t.Errorf("vary=%q (缺 Origin，缓存会串源)", rr.Header().Get("Vary"))
	}
}

// TestCORSIgnoresNonBrowser 无 Origin（curl / 服务端调用）不加 CORS 头，行为零变化。
func TestCORSIgnoresNonBrowser(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq("POST", "/v1/images/generations", `{"model":"cn:hunyuan-image-alpha","prompt":"a cat"}`))

	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("unexpected allow-origin=%q for non-browser request", got)
	}
}
