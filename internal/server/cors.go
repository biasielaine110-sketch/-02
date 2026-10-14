package server

import "net/http"

// corsAllowedHeaders 预检未指定 Access-Control-Request-Headers 时回落的放行头名单。
// 覆盖 OpenAI 兼容客户端与浏览器前端的常见请求头（鉴权 / 内容类型 / 会话追踪 / 缓存协商）。
const corsAllowedHeaders = "Authorization, Content-Type, Accept, Origin, X-Requested-With, " +
	"X-Conversation-Request-ID, X-Trace-ID, Cache-Control, Pragma"

// corsMaxAge 预检结果缓存时长（秒）。取 24h，减少浏览器对同一路由的重复 OPTIONS。
const corsMaxAge = "86400"

// applyCORS 给响应补 CORS 头，并在预检请求上直接短路 204。
// 返回 true 表示请求已处理完（调用方不应再进 mux）。
//
// 背景：网关面向**浏览器前端**（「Nww-无限画布」等 Pages/Vite 应用）直连的
// OpenAI 兼容 API。跨域请求浏览器会先发 OPTIONS 预检，而 Go 1.22 ServeMux 对
// 未注册的方法回 405（Allow: POST）——预检直接失败，浏览器判定为 cross-origin
// block，真实 POST 根本不发出（画布侧表现为「请求被浏览器跨域策略拦截」，
// axios 报 ERR_NETWORK）。
//
// 之所以放在 ServeHTTP 最外层而不是各 handler：预检请求**不带 Authorization**，
// 必须在 withAuth 鉴权中间件之前处理；且该行为对全部 /v1 路由一致。
//
// 放行策略：反射 Origin（等价于 *，但兼容带凭据场景）。网关是 API Key 鉴权的
// 公开 API，CORS 本身不构成安全边界（非浏览器调用不受 CORS 约束），
// 「任意来源可用」是预期行为——真正的门禁是 Bearer Key。
func applyCORS(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// 非浏览器 / 同源请求：无 CORS 语义，原样放行（curl、服务端调用零影响）。
		return false
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS, HEAD")
	if reqHeaders := r.Header.Get("Access-Control-Request-Headers"); reqHeaders != "" {
		// 回显浏览器声明要用的头（含 Authorization），避免白名单遗漏导致预检失败。
		h.Set("Access-Control-Allow-Headers", reqHeaders)
	} else {
		h.Set("Access-Control-Allow-Headers", corsAllowedHeaders)
	}
	h.Set("Access-Control-Max-Age", corsMaxAge)
	// Vary 供各级缓存正确按来源分桶（Add 以免覆盖其他 Vary 值）。
	h.Add("Vary", "Origin")
	h.Add("Vary", "Access-Control-Request-Headers")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent) // 预检到此为止，不进 mux（mux 对 OPTIONS 回 405）
		return true
	}
	return false
}
