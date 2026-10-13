package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// mediaRecorder 记录假上游收到的请求，供断言头族/请求体形态。
type mediaRecorder struct {
	imageBody string
	imageAuth string
	tcAuth    string
	submitRaw string
	queryRaw  string

	// miora 通道
	apiKeyAuth    string
	mioraAuth     string
	mioraSubmit   string
	mioraQueryRaw string
}

// mediaFakeUpstream 按路径 + X-TC-Action 路由的假上游：
//   - /v2/images/generations              → 生图信封（code=0 + data.data[].url）
//   - /agenttool/v1/tcproxy               → 视频提交/查询（TC3 签名头 + Response 包裹）
//   - /v2/api-keys                        → miora 的 ck_t_ 临时密钥签发
//   - /api/ai/workbuddy-proxy/video/*     → miora（MiniMax H3）视频提交/查询
func mediaFakeUpstream(t *testing.T, rec *mediaRecorder) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			var status int
			var body string
			switch {
			case strings.HasSuffix(r.URL.Path, "/v2/images/generations"):
				rec.imageBody = string(raw)
				rec.imageAuth = r.Header.Get("Authorization")
				status = 200
				body = `{"code":0,"msg":"","data":{"created":1,"data":[{"url":"https://cos.example/a.png","revised_prompt":"a cat"}],"usage":{"total_tokens":12,"credit":5.71}}}`
			case strings.HasSuffix(r.URL.Path, "/v2/api-keys"):
				rec.apiKeyAuth = r.Header.Get("Authorization")
				status = 200
				body = `{"code":0,"msg":"OK","data":{"key":"ck_t_test.abc","expires_at":"2026-10-13T23:00:00+08:00"}}`
			case strings.HasSuffix(r.URL.Path, "/agenttool/v1/tcproxy"):
				rec.tcAuth = r.Header.Get("Authorization")
				switch r.Header.Get("X-TC-Action") {
				case "SubmitAigcVideoJob":
					rec.submitRaw = string(raw)
					status = 200
					body = `{"Response":{"JobId":"job-123","RequestId":"req-1"}}`
				case "DescribeAigcVideoJob":
					rec.queryRaw = string(raw)
					status = 200
					body = `{"Response":{"Status":"DONE","ResultUrl":"https://cos.example/v.mp4","RequestId":"req-2"}}`
				default:
					status, body = 400, `{"Response":{"Error":{"Code":"InvalidAction"}}}`
				}
			case strings.HasSuffix(r.URL.Path, "/api/ai/workbuddy-proxy/video/submit"):
				rec.mioraAuth = r.Header.Get("Authorization")
				rec.mioraSubmit = string(raw)
				status = 200
				body = `{"code":0,"failed":false,"data":{"upstreamTaskId":"miora-task-1","model":"minimax-h3-iOA","provider":"copilot-video"},"message":"successful","httpStatus":200}`
			case strings.HasSuffix(r.URL.Path, "/api/ai/workbuddy-proxy/video/query-task"):
				rec.mioraAuth = r.Header.Get("Authorization")
				rec.mioraQueryRaw = string(raw)
				status = 200
				body = `{"code":0,"failed":false,"data":{"miora-task-1":{"status":"completed","resultFiles":[{"signedUrl":"https://cos.example/miora.mp4","mimeType":"video/mp4"}]}},"message":"successful","httpStatus":200}`
			default:
				status, body = 404, `no route`
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
		MioraBase:     "https://miora.example",
	}
}

func mediaHandler(t *testing.T, rec *mediaRecorder) *Handler {
	t.Helper()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	return NewHandler(Config{Pool: p, Upstream: mediaFakeUpstream(t, rec), APIKey: "sk-test"})
}

func authedReq(method, path, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer sk-test")
	return r
}

// TestImagesGenerations 生图同步链路：编码前缀剥离 + 上游 url 透出 + usage 透出。
func TestImagesGenerations(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)

	req := authedReq("POST", "/v1/images/generations", `{"model":"cn:hunyuan-image-alpha","prompt":"a cat"}`)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Data []struct {
			URL           string `json:"url"`
			RevisedPrompt string `json:"revised_prompt"`
		} `json:"data"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if len(resp.Data) != 1 || resp.Data[0].URL != "https://cos.example/a.png" {
		t.Fatalf("data=%+v", resp.Data)
	}
	if resp.Data[0].RevisedPrompt != "a cat" {
		t.Errorf("revised_prompt=%q", resp.Data[0].RevisedPrompt)
	}
	if resp.Usage == nil {
		t.Error("usage missing")
	}
	// 前缀剥离：上游应收到裸模型名，而不是 "cn:hunyuan-image-alpha"。
	if !strings.Contains(rec.imageBody, `"model":"hunyuan-image-alpha"`) {
		t.Errorf("upstream body=%s", rec.imageBody)
	}
	if rec.imageAuth != "Bearer at1" {
		t.Errorf("upstream auth=%q", rec.imageAuth)
	}
}

func TestImagesGenerationsValidation(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)

	// 缺 prompt → 400
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq("POST", "/v1/images/generations", `{"prompt":"  "}`))
	if rr.Code != 400 {
		t.Fatalf("blank prompt code=%d want 400", rr.Code)
	}

	// 无鉴权 → 401
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(`{"prompt":"x"}`)))
	if rr2.Code != 401 {
		t.Fatalf("no auth code=%d want 401", rr2.Code)
	}
}

// TestVideosGenerationsFlow 生视频异步链路：提交取 JobId、轮询取产物、未知任务 404。
func TestVideosGenerationsFlow(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)

	// 提交
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq("POST", "/v1/videos/generations", `{"model":"cn:kling-v2.6","prompt":"a dog"}`))
	if rr.Code != 200 {
		t.Fatalf("submit code=%d body=%s", rr.Code, rr.Body.String())
	}
	var sub struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	json.Unmarshal(rr.Body.Bytes(), &sub)
	if sub.ID != "job-123" || sub.Status != "submitted" {
		t.Fatalf("submit resp=%+v", sub)
	}
	// 目录 ID 拆成 vendor/model 落到上游
	if !strings.Contains(rec.submitRaw, `"Vendor":"Kling"`) || !strings.Contains(rec.submitRaw, `"Model":"v2.6"`) {
		t.Errorf("submit body=%s", rec.submitRaw)
	}
	// TC3 授权头：secret_id = "<provider>.<accessToken>"
	if !strings.HasPrefix(rec.tcAuth, "TC3-HMAC-SHA256 Credential=video-effect.at1/") {
		t.Errorf("tc auth=%q", rec.tcAuth)
	}

	// 查询（同一账号）
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, authedReq("GET", "/v1/videos/generations/job-123", ""))
	if rr2.Code != 200 {
		t.Fatalf("query code=%d body=%s", rr2.Code, rr2.Body.String())
	}
	var q struct {
		Status string `json:"status"`
		URL    string `json:"url"`
	}
	json.Unmarshal(rr2.Body.Bytes(), &q)
	if q.Status != "succeeded" || q.URL != "https://cos.example/v.mp4" {
		t.Fatalf("query resp=%+v", q)
	}

	// 未知任务 → 404（进程内任务表未命中）
	rr3 := httptest.NewRecorder()
	h.ServeHTTP(rr3, authedReq("GET", "/v1/videos/generations/nope", ""))
	if rr3.Code != 404 {
		t.Fatalf("unknown job code=%d want 404", rr3.Code)
	}
}

func TestParseVideoModel(t *testing.T) {
	cases := []struct{ in, vendor, model string }{
		{"kling-v2.6", "Kling", "v2.6"},
		{"Kling-2.0", "Kling", "2.0"},
		{"kling", "Kling", ""},
		{"miora-minimax-h3", "Miora", "minimax-h3"},
		{"MIORA-minimax-h3", "Miora", "minimax-h3"},
		{"miora", "Miora", ""},
		{"v2.6", "", "v2.6"},
		{"", "", ""},
		{"  kling-v2.6  ", "Kling", "v2.6"},
	}
	for _, c := range cases {
		v, m := parseVideoModel(c.in)
		if v != c.vendor || m != c.model {
			t.Errorf("parseVideoModel(%q)=(%q,%q) want (%q,%q)", c.in, v, m, c.vendor, c.model)
		}
	}
}

// TestVideosGenerationsMioraFlow miora（MiniMax H3）异步链路：
// 目录 ID → vendor 路由 → 自签 ck_t_ → 提交取 upstreamTaskId → 查询取产物。
func TestVideosGenerationsMioraFlow(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq("POST", "/v1/videos/generations",
		`{"model":"cn:miora-minimax-h3","prompt":"a dog","duration":6,"resolution":"768p","aspect_ratio":"16:9"}`))
	if rr.Code != 200 {
		t.Fatalf("submit code=%d body=%s", rr.Code, rr.Body.String())
	}
	var sub struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	json.Unmarshal(rr.Body.Bytes(), &sub)
	if sub.ID != "miora-task-1" || sub.Status != "submitted" {
		t.Fatalf("submit resp=%+v", sub)
	}
	// 提交体：prompt/duration/resolution(归一为大写)/aspect_ratio 四项。
	if !strings.Contains(rec.mioraSubmit, `"prompt":"a dog"`) ||
		!strings.Contains(rec.mioraSubmit, `"duration":6`) ||
		!strings.Contains(rec.mioraSubmit, `"resolution":"768P"`) ||
		!strings.Contains(rec.mioraSubmit, `"aspect_ratio":"16:9"`) {
		t.Errorf("miora submit body=%s", rec.mioraSubmit)
	}
	// 鉴权：先签 ck_t_（用账号 accessToken），媒体端点用 ck_t_。
	if rec.apiKeyAuth != "Bearer at1" {
		t.Errorf("api-keys auth=%q", rec.apiKeyAuth)
	}
	if rec.mioraAuth != "Bearer ck_t_test.abc" {
		t.Errorf("miora auth=%q", rec.mioraAuth)
	}

	// 查询（走 miora 后端）
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, authedReq("GET", "/v1/videos/generations/miora-task-1", ""))
	if rr2.Code != 200 {
		t.Fatalf("query code=%d body=%s", rr2.Code, rr2.Body.String())
	}
	var q struct {
		Status string `json:"status"`
		URL    string `json:"url"`
	}
	json.Unmarshal(rr2.Body.Bytes(), &q)
	if q.Status != "succeeded" || q.URL != "https://cos.example/miora.mp4" {
		t.Fatalf("query resp=%+v", q)
	}
	if !strings.Contains(rec.mioraQueryRaw, `"upstreamTaskIds":["miora-task-1"]`) {
		t.Errorf("miora query body=%s", rec.mioraQueryRaw)
	}
}

// TestVideosGenerationsMioraValidation miora 规格闸门在本地拦截（省一次上游往返）。
func TestVideosGenerationsMioraValidation(t *testing.T) {
	cases := []string{
		`{"model":"cn:miora-minimax-h3","prompt":"a dog","duration":2}`,         // 时长 < 4
		`{"model":"cn:miora-minimax-h3","prompt":"a dog","duration":20}`,        // 时长 > 15
		`{"model":"cn:miora-minimax-h3","prompt":"a dog","resolution":"999p"}`,  // 分辨率不在白名单
		`{"model":"cn:miora-minimax-h3","prompt":"a dog","aspect_ratio":"5:4"}`, // 画幅不在白名单
	}
	for _, body := range cases {
		rec := &mediaRecorder{}
		h := mediaHandler(t, rec)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, authedReq("POST", "/v1/videos/generations", body))
		if rr.Code != 400 {
			t.Errorf("body=%s code=%d want 400", body, rr.Code)
		}
		if rec.mioraSubmit != "" {
			t.Errorf("body=%s should be rejected locally, but upstream saw submit", body)
		}
	}
}

// TestMediaModelEntriesInCatalog 媒体模型登记进 /v1/models 目录（带 type 标记）。
func TestMediaModelEntriesInCatalog(t *testing.T) {
	entries := mediaModelEntries()
	if len(entries) != 3 {
		t.Fatalf("want 3 media entries, got %d", len(entries))
	}
	want := map[string]string{
		"cn:" + upstream.DefaultImageModel:       "image",
		"cn:kling-" + upstream.DefaultVideoModel: "video",
		"cn:miora-" + upstream.MioraVideoModel:   "video",
	}
	for _, e := range entries {
		id, _ := e["id"].(string)
		typ, _ := e["type"].(string)
		if want[id] != typ {
			t.Errorf("entry id=%q type=%q not in catalog expectation", id, typ)
		}
		delete(want, id)
	}
	if len(want) != 0 {
		t.Errorf("missing media entries: %v", want)
	}
}

// TestVideosGenerationsMioraFrameFlow 首尾帧模式：字符串/对象两种帧取值都落到上游 input。
func TestVideosGenerationsMioraFrameFlow(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq("POST", "/v1/videos/generations",
		`{"model":"cn:miora-minimax-h3","prompt":"morph","duration":5,`+
			`"first_frame":"https://x/a.png","last_frame":{"base64":"data:image/png;base64,QQ=="}}`))
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rec.mioraSubmit, `"first_frame_image":{"url":"https://x/a.png"}`) {
		t.Errorf("first frame not forwarded: %s", rec.mioraSubmit)
	}
	// data URI 前缀应被剥离成裸 base64。
	if !strings.Contains(rec.mioraSubmit, `"last_frame_image":{"base64":"QQ=="}`) {
		t.Errorf("last frame not normalized: %s", rec.mioraSubmit)
	}
}

// TestVideosGenerationsMioraReferenceFlow 参考生模式：数组逐项转成 MediaPayload。
func TestVideosGenerationsMioraReferenceFlow(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq("POST", "/v1/videos/generations",
		`{"model":"cn:miora-minimax-h3","prompt":"x","references":["https://x/1.png","https://x/2.png"]}`))
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rec.mioraSubmit, `"reference_images":[{"url":"https://x/1.png"},{"url":"https://x/2.png"}]`) {
		t.Errorf("references not forwarded: %s", rec.mioraSubmit)
	}
}

// TestVideosGenerationsMioraVisualValidation 非法图入参本地拦截（不发上游）：
// 空参考数组 / 缺首帧 / 空载荷 / 两种模式并存 / 非 miora 通道携带帧字段。
func TestVideosGenerationsMioraVisualValidation(t *testing.T) {
	cases := []string{
		`{"model":"cn:miora-minimax-h3","prompt":"x","references":[]}`,
		`{"model":"cn:miora-minimax-h3","prompt":"x","last_frame":"https://x/a.png"}`,
		`{"model":"cn:miora-minimax-h3","prompt":"x","first_frame":{}}`,
		`{"model":"cn:miora-minimax-h3","prompt":"x","first_frame":"https://x/a.png","references":["https://x/1.png"]}`,
		`{"model":"cn:kling-v2.6","prompt":"x","first_frame":"https://x/a.png"}`,
	}
	for _, body := range cases {
		rec := &mediaRecorder{}
		h := mediaHandler(t, rec)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, authedReq("POST", "/v1/videos/generations", body))
		if rr.Code != 400 {
			t.Errorf("body=%s code=%d want 400 (resp=%s)", body, rr.Code, rr.Body.String())
		}
		if rec.mioraSubmit != "" || rec.submitRaw != "" {
			t.Errorf("body=%s should be rejected locally, but upstream saw a submit", body)
		}
	}
}
