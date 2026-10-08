package panel

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAppJSSyntax app.js 必须能通过 JS 解析器语法校验。
//
// 为什么需要：app.js 是 go:embed 进二进制的静态资源，Go 编译器不检查其内容——
// 一次对象字面量键名未加引号（Model_chat_GLM5.2 被解析成属性访问 + 数字字面量）
// 就让整个面板白屏，而所有 Go 测试依然全绿。此测试把语法校验前移到 CI。
// 无 node 环境时跳过（不阻塞无 Node 的构建机）。
func TestAppJSSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS syntax check")
	}
	path, err := filepath.Abs("app.js")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "--check", path).CombinedOutput()
	if err != nil {
		t.Fatalf("app.js syntax error:\n%s", out)
	}
}

// TestStaticAssetsAreRevalidated 面板静态资源必须带 Cache-Control + ETag。
//
// 为什么需要：index.html / app.js 的 URL 不带版本号，一旦没有校验头，浏览器就会一直复用旧副本。
// 真实踩到过——新增按钮、重启网关后服务端返回的 HTML 已经含按钮，页面上却看不到，
// 排查方向被带到了"功能没生效"上。此测试把"必须可校验"钉住。
func TestStaticAssetsAreRevalidated(t *testing.T) {
	p := newTestPanel()
	for _, path := range []string{"/panel/", "/panel/app.js"} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: code=%d", path, rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
			t.Errorf("%s: Cache-Control=%q, want it to contain no-cache", path, cc)
		}
		etag := rec.Header().Get("ETag")
		if etag == "" {
			t.Fatalf("%s: missing ETag", path)
		}

		// 回传同一 ETag → 304，且不带正文。
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("If-None-Match", etag)
		rec2 := httptest.NewRecorder()
		p.ServeHTTP(rec2, req)
		if rec2.Code != http.StatusNotModified {
			t.Errorf("%s: revalidate code=%d, want 304", path, rec2.Code)
		}
		if rec2.Body.Len() != 0 {
			t.Errorf("%s: 304 must carry no body, got %d bytes", path, rec2.Body.Len())
		}

		// 过期 ETag → 200（必须拿到新版内容）。
		req3 := httptest.NewRequest("GET", path, nil)
		req3.Header.Set("If-None-Match", `"stale"`)
		rec3 := httptest.NewRecorder()
		p.ServeHTTP(rec3, req3)
		if rec3.Code != http.StatusOK {
			t.Errorf("%s: stale ETag code=%d, want 200", path, rec3.Code)
		}
		if rec3.Body.Len() == 0 {
			t.Errorf("%s: 200 must carry the body", path)
		}
	}
}

// TestStaticETagsDifferPerResource 两份资源内容不同，ETag 必须不同。
func TestStaticETagsDifferPerResource(t *testing.T) {
	p := newTestPanel()
	get := func(path string) string {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec.Header().Get("ETag")
	}
	a, b := get("/panel/"), get("/panel/app.js")
	if a == "" || b == "" {
		t.Fatal("ETag must not be empty")
	}
	if a == b {
		t.Errorf("index.html and app.js share the same ETag %q", a)
	}
}

// TestIndexHTMLNoInlineScript index.html 不得含内联 <script> 块：
// 严格 CSP（script-src 'self'）会拦截内联脚本，页面将完全不可用。
// 外链形式 <script src="..."> 允许。
func TestIndexHTMLNoInlineScript(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	rest := body
	for {
		idx := strings.Index(rest, "<script")
		if idx < 0 {
			break
		}
		rest = rest[idx:]
		end := strings.Index(rest, ">")
		if end < 0 {
			break
		}
		tag := rest[:end+1]
		if !strings.Contains(tag, "src=") {
			t.Fatalf("index.html contains inline <script> (blocked by CSP): %s", tag)
		}
		rest = rest[end:]
	}
}
