// index.go 面板静态资源与安全响应头。
//
// 资源经 go:embed 打进二进制（随服务部署，无外部构建步骤）：
//   - index.html  页面骨架
//   - app.js      全部前端逻辑（独立文件而非内联，为了启用无需 unsafe-inline 的严格 CSP）
//
// 安全头对"面板页面与全部 /panel/api/* 响应"统一生效：CSP 限制脚本只能来自本服务，
// 禁止被 iframe 嵌套（防点击劫持），禁 MIME 嗅探，并声明不泄露 Referer 出去。
package panel

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
)

//go:embed index.html
var indexHTML []byte

//go:embed app.js
var appJS []byte

// indexETag / appETag 按内容算出的强 ETag（首次请求时算一次，之后复用）。
var (
	indexETag = sync.OnceValue(func() string { return contentETag(indexHTML) })
	appETag   = sync.OnceValue(func() string { return contentETag(appJS) })
)

// contentETag 取内容哈希前 16 字节做 ETag：够区分版本，又比全量短。
func contentETag(b []byte) string {
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// etagMatches 判断 If-None-Match 是否命中当前 ETag：支持 "*"、弱校验前缀 W/、以及逗号分隔的多值。
func etagMatches(header, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		v := strings.TrimSpace(part)
		if v == "*" {
			return true
		}
		if strings.TrimPrefix(v, "W/") == etag {
			return true
		}
	}
	return false
}

// serveStatic 输出内嵌静态资源，带 ETag 并强制每次回源校验。
//
// 为什么必须显式声明：面板资源的 URL 不带版本号，若不加校验头，浏览器会按启发式规则
// 一直复用旧副本——改完代码、重启网关后页面上仍是老界面，看起来"功能没生效"。
// 实测踩到：新增账号池迁移按钮后，服务端返回的 HTML 已含按钮，浏览器却仍显示旧页面。
//
// 用 no-cache 而不是 no-store：允许缓存，但每次使用前必须回来校验；
// 内容没变就回 304（省一次 54KB/85KB 的重传），变了就自动拿到新版。
func serveStatic(w http.ResponseWriter, r *http.Request, body []byte, etag, ctype string) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", etag)
	if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatches(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// csp 内容安全策略（严格版，无需 unsafe-inline）：
//   - default-src 'none'        默认全禁，逐个开口
//   - script-src 'self'         只跑同源脚本（app.js）；页面无内联事件处理器/内联脚本
//   - style-src 'self' 'unsafe-inline'
//     style 的内联是设计取舍：页面有少量 style="..." 属性（进度条宽度、表格列宽），
//     允许内联样式不会导致脚本执行；仍禁止外部样式域与 @import 外链。
//   - connect-src 'self'        前端 fetch 只能打本服务
//   - img-src 'self' data:      图标/内联图
//   - form-action 'none'        页面无表单提交目标（配置页是 JS 提交）
//   - frame-ancestors 'none'    禁止被任何站点 iframe 嵌套（点击劫持）
//   - base-uri 'none'          禁止注入 <base> 改写相对路径
const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"connect-src 'self'; img-src 'self' data:; form-action 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// setSecurityHeaders 写入面板统一安全响应头（页面与 API 都要，API 也含 JSON 数据）。
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff") // 禁 MIME 嗅探
	w.Header().Set("X-Frame-Options", "DENY")           // 老浏览器兜底（CSP frame-ancestors 的等价项）
	w.Header().Set("Referrer-Policy", "no-referrer")    // 不外泄面板地址给外部站点
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
}

// index 输出面板页面（静态无秘密；数据接口 /panel/api/* 才走鉴权）。
func (p *Panel) index(w http.ResponseWriter, r *http.Request) {
	serveStatic(w, r, indexHTML, indexETag(), "text/html; charset=utf-8")
}

// appScript 输出前端逻辑（同源脚本，供 CSP script-src 'self' 加载）。
func (p *Panel) appScript(w http.ResponseWriter, r *http.Request) {
	serveStatic(w, r, appJS, appETag(), "text/javascript; charset=utf-8")
}
