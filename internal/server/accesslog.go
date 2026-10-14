package server

import (
	"log"
	"net/http"
	"strings"
	"time"
)

// 媒体请求（生图 / 改图 / 生视频）的一行访问日志。
//
// 为什么单独给媒体加日志：媒体端点此前**不落任何日志**——chat 有自己的逐请求
// 统计表（chatStat），媒体却没有。于是每当前端（「Nww-无限画布」等）报错
// （跨域拦截 / Cloudflare 502 / 生成失败），都无从判断「请求到底有没有到网关、
// 网关回了什么状态码」，只能在网络层反复猜。
// 本日志就是回答这一个问题：
//   - 有 `[media]` 行 → 请求到过网关，行内的状态码就是网关的真实答复；
//   - 无 `[media]` 行 → 请求根本没到网关，问题在链路（Cloudflare 代理 / Funnel / 前端）。
//
// 覆盖 /v1/images/*、/v1/videos*、/v1/files*、/v1/uploads*，chat 走既有统计表，不重复记录。
//
// /v1/files 与 /v1/uploads 必须一起覆盖：画布的视频链路**在提交生成之前**先打
// `/v1/files/upload`，这一步 404 时如果没日志，现场只剩浏览器控制台一行 404，
// 会让人误以为是链路问题（见 files.go 顶部注释）。
func isMediaPath(p string) bool {
	return strings.HasPrefix(p, "/v1/images") || strings.HasPrefix(p, "/v1/videos") ||
		strings.HasPrefix(p, "/v1/files") || strings.HasPrefix(p, "/v1/uploads")
}

// statusRecorder 记录已写出的状态码（未显式 WriteHeader 时按 net/http 语义视为 200）。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Flush 透传，避免包装后丢失 http.Flusher（媒体响应目前是纯 JSON，无流式，
// 但保持接口完整以防后续加分块输出）。
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// logMediaAccess 包一层状态码记录，返回包装后的 writer 与「写日志」的收尾函数。
// 收尾函数需在 handler 返回后调用（defer），此时状态码已定型。
func logMediaAccess(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, func()) {
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	start := time.Now()
	return rec, func() {
		log.Printf("[media] %s %s -> %d in %s ip=%s ua=%q",
			r.Method, r.URL.Path, rec.status,
			time.Since(start).Round(time.Millisecond),
			mediaClientIP(r), r.UserAgent())
	}
}

// mediaClientIP 取请求来源 IP：优先 X-Forwarded-For 首个地址（画布经 Cloudflare
// 代理转发时会带上，可据此区分「经代理」与「浏览器直连」），否则回落 RemoteAddr。
func mediaClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	return r.RemoteAddr
}
