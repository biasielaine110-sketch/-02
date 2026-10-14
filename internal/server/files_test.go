package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// multipartUploadReq 构造一次文件上传请求（画布 files/upload 的原样形态：字段名 "file"）。
func multipartUploadReq(t *testing.T, fieldName, fileName, content string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile(fieldName, fileName)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write([]byte(content)); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	req := httptest.NewRequest("POST", "/v1/files/upload", &buf)
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// pngHeader 一个最小 PNG 头（8 字节签名），让 http.DetectContentType 认出 image/png。
const pngHeader = "\x89PNG\r\n\x1a\n"

// TestFilesUploadThenServe 上传-回链-取回闭环：
// 上传拿到的 URL 必须能被 GET 原样取回（这是画布链路成立的前提）。
func TestFilesUploadThenServe(t *testing.T) {
	h := mediaHandler(t, &mediaRecorder{})

	content := pngHeader + "fake-png-bytes"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, multipartUploadReq(t, "file", "ref.png", content))
	if rr.Code != 200 {
		t.Fatalf("upload code=%d body=%s", rr.Code, rr.Body.String())
	}
	out := decodeJSONObj(t, rr)
	url, _ := out["url"].(string)
	if !strings.HasPrefix(url, "http") || !strings.Contains(url, "/v1/files/") {
		t.Fatalf("url=%q body=%s", url, rr.Body.String())
	}
	// 画布读的是 data.url || data.data.url，两种外壳都要给。
	data, _ := out["data"].(map[string]any)
	if data == nil || data["url"] != url {
		t.Fatalf("data.url missing: %s", rr.Body.String())
	}
	if out["success"] != true {
		t.Fatalf("success=%v (uploads/images 形态按 success==false 判失败)", out["success"])
	}

	// 取回：路径取自回链，GET 不带 Authorization（上游抓取器带不了 Bearer）。
	path := url[strings.Index(url, "/v1/files/"):]
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, httptest.NewRequest("GET", path, nil))
	if rr2.Code != 200 {
		t.Fatalf("get code=%d body=%s", rr2.Code, rr2.Body.String())
	}
	if rr2.Body.String() != content {
		t.Fatalf("bytes mismatch: got %q", rr2.Body.String())
	}
	if ct := rr2.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/png") {
		t.Fatalf("content-type=%q", ct)
	}
}

// handlerWithPublicBase 同 mediaHandler，但显式配公网基址（验证回链用的是配置值）。
func handlerWithPublicBase(t *testing.T, base string) *Handler {
	t.Helper()
	return NewHandler(Config{
		Pool:          testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:      mediaFakeUpstream(t, &mediaRecorder{}),
		APIKey:        "sk-test",
		PublicBaseURL: base,
	})
}

// TestFilesUploadPathIsPublicBaseURL 配了 server.public_base_url 时回链用配置值
// （反向代理改写 Host 的场景，靠请求头推断会给出内网地址）。
func TestFilesUploadPathIsPublicBaseURL(t *testing.T) {
	h := handlerWithPublicBase(t, "https://wb2api.example.ts.net/")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, multipartUploadReq(t, "file", "ref.png", pngHeader+"x"))
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	url, _ := decodeJSONObj(t, rr)["url"].(string)
	if !strings.HasPrefix(url, "https://wb2api.example.ts.net/v1/files/") {
		t.Fatalf("url=%q", url)
	}
}

// TestFilesUploadNoFile 没带文件必须 400（不是静默成功回一个空文件）。
func TestFilesUploadNoFile(t *testing.T) {
	h := mediaHandler(t, &mediaRecorder{})
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "x")
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/v1/files/upload", &buf)
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", mw.FormDataContentType())

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
}

// TestFilesGetUnknownID 未知名/过期 id → 404（不泄露内部状态）。
func TestFilesGetUnknownID(t *testing.T) {
	h := mediaHandler(t, &mediaRecorder{})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/files/deadbeef", nil))
	if rr.Code != 404 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
}

// TestOwnFileID 只认本网关的 /v1/files/{id} 路径，且拒绝带路径穿越的 id。
func TestOwnFileID(t *testing.T) {
	cases := []struct {
		in      string
		wantID  string
		wantHit bool
	}{
		{"https://wb2api.example.ts.net/v1/files/abc123", "abc123", true},
		{"http://127.0.0.1:7863/v1/files/abc123?x=1", "", false},
		{"https://cdn.example.com/img.png", "", false},
		{"https://wb2api.example.ts.net/v1/files/a/b", "", false},
		{"https://wb2api.example.ts.net/v1/uploads/images", "", false},
	}
	for _, c := range cases {
		id, ok := ownFileID(c.in)
		if ok != c.wantHit || (ok && id != c.wantID) {
			t.Errorf("ownFileID(%q) = (%q,%v), want (%q,%v)", c.in, id, ok, c.wantID, c.wantHit)
		}
	}
}

// TestVideoCompatOwnFileURLMaterialized 画布链路端到端（离线）：
// 先上传参考图拿到回链，再把回链当 input_reference.image_url 提交视频——
// 网关应把该回链物化成 base64 送上游（而不是把自家 URL 透给上游去回源抓）。
func TestVideoCompatOwnFileURLMaterialized(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, multipartUploadReq(t, "file", "ref.png", pngHeader+"hello-frame"))
	if rr.Code != 200 {
		t.Fatalf("upload code=%d body=%s", rr.Code, rr.Body.String())
	}
	imgURL, _ := decodeJSONObj(t, rr)["url"].(string)

	body := `{"model":"cn:miora-minimax-h3","prompt":"a cat","duration":"5",` +
		`"input_reference":{"image_url":"` + imgURL + `"}}`
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, authedReq("POST", "/v1/videos", body))
	if rr2.Code != 200 {
		t.Fatalf("video code=%d body=%s", rr2.Code, rr2.Body.String())
	}

	var sent struct {
		Input struct {
			FirstFrame *struct {
				Base64 string `json:"base64"`
				URL    string `json:"url"`
			} `json:"first_frame_image"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(rec.mioraSubmit), &sent); err != nil {
		t.Fatalf("decode miora submit: %v body=%s", err, rec.mioraSubmit)
	}
	if sent.Input.FirstFrame == nil {
		t.Fatalf("first_frame_image missing: %s", rec.mioraSubmit)
	}
	if sent.Input.FirstFrame.URL != "" {
		t.Fatalf("own url leaked to upstream: %s", rec.mioraSubmit)
	}
	want := base64.StdEncoding.EncodeToString([]byte(pngHeader + "hello-frame"))
	if sent.Input.FirstFrame.Base64 != want {
		t.Fatalf("base64 mismatch: got %q want %q", sent.Input.FirstFrame.Base64, want)
	}
}

// TestVideoCompatFirstFrameImageAlias 画布第三套兜底请求体用 first_frame_image
// （miora 原生叫法）；认不出就会把图生视频静默降级成文生视频。
func TestVideoCompatFirstFrameImageAlias(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)

	body := `{"model":"cn:miora-minimax-h3","prompt":"a cat","duration":5,` +
		`"first_frame_image":"https://x.example/first.png","last_frame_image":"https://x.example/last.png"}`
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq("POST", "/v1/videos", body))
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	var sent struct {
		Input struct {
			FirstFrame *struct {
				URL string `json:"url"`
			} `json:"first_frame_image"`
			LastFrame *struct {
				URL string `json:"url"`
			} `json:"last_frame_image"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(rec.mioraSubmit), &sent); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.mioraSubmit)
	}
	if sent.Input.FirstFrame == nil || sent.Input.FirstFrame.URL != "https://x.example/first.png" {
		t.Fatalf("first_frame_image=%+v body=%s", sent.Input.FirstFrame, rec.mioraSubmit)
	}
	if sent.Input.LastFrame == nil || sent.Input.LastFrame.URL != "https://x.example/last.png" {
		t.Fatalf("last_frame_image=%+v body=%s", sent.Input.LastFrame, rec.mioraSubmit)
	}
}

// TestVideoCompatMultipartFirstFrameImageFile first_frame_image 以**文件**形态给出
// （部分兜底形态会这么发）→ 首帧生效，而不是被当成参考图数组。
func TestVideoCompatMultipartFirstFrameImageFile(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "cn:miora-minimax-h3")
	_ = mw.WriteField("prompt", "a cat")
	_ = mw.WriteField("duration", "5")
	fw, err := mw.CreateFormFile("first_frame_image", "f.png")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	_, _ = fw.Write([]byte("rawframe"))
	_ = mw.Close()

	req := httptest.NewRequest("POST", "/v1/videos", &buf)
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", mw.FormDataContentType())

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	var sent struct {
		Input struct {
			FirstFrame *struct {
				Base64 string `json:"base64"`
			} `json:"first_frame_image"`
			ReferenceImages []struct{} `json:"reference_images"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(rec.mioraSubmit), &sent); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.mioraSubmit)
	}
	want := base64.StdEncoding.EncodeToString([]byte("rawframe"))
	if sent.Input.FirstFrame == nil || sent.Input.FirstFrame.Base64 != want {
		t.Fatalf("first_frame=%+v body=%s", sent.Input.FirstFrame, rec.mioraSubmit)
	}
	if len(sent.Input.ReferenceImages) != 0 {
		t.Fatalf("should not become references: %s", rec.mioraSubmit)
	}
}

// TestIsMediaPathCoversUploads 上传端点必须落 [media] 访问日志——
// 这次现场的唯一线索就是浏览器控制台那行 404，网关侧反而什么都没记。
func TestIsMediaPathCoversUploads(t *testing.T) {
	for _, p := range []string{"/v1/files/upload", "/v1/files/abc", "/v1/uploads/images",
		"/v1/images/generations", "/v1/videos"} {
		if !isMediaPath(p) {
			t.Errorf("isMediaPath(%q) = false, want true", p)
		}
	}
	if isMediaPath("/v1/chat/completions") {
		t.Error("chat path must not be double-logged")
	}
}
