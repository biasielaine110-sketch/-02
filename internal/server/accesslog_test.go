package server

import (
	"bytes"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMediaAccessLog 媒体请求落一行访问日志（含状态码），非媒体路径不落。
// 该日志是「请求有没有到达网关」的唯一判据（此前媒体端点完全不落日志）。
func TestMediaAccessLog(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq("POST", "/v1/images/generations",
		`{"model":"cn:hunyuan-image-alpha","prompt":"a cat"}`))
	if !strings.Contains(buf.String(), "[media] POST /v1/images/generations -> 200") {
		t.Fatalf("media path must be logged with status: %q", buf.String())
	}

	// 鉴权失败也要留痕（401 是「请求到了网关但 Key 不对」的判据）。
	buf.Reset()
	badReq := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(`{"prompt":"x"}`))
	rrBad := httptest.NewRecorder()
	h.ServeHTTP(rrBad, badReq)
	if !strings.Contains(buf.String(), "-> 401") {
		t.Fatalf("unauthorized media request must still be logged: %q", buf.String())
	}

	// 非媒体路径不落（chat 走自己的统计表，避免重复）。
	// 用 /healthz：不触达上游，避免干扰假上游。
	buf.Reset()
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, authedReq("GET", "/healthz", ""))
	if strings.Contains(buf.String(), "[media]") {
		t.Fatalf("non-media path must not be logged: %q", buf.String())
	}
}
