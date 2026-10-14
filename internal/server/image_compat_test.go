package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// decodeImageURL 取 OpenAI 生图响应里的第一张 url。
func decodeImageURL(t *testing.T, body string) string {
	t.Helper()
	var out struct {
		Created int64 `json:"created"`
		Data    []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("parse resp: %v body=%s", err, body)
	}
	if len(out.Data) == 0 {
		t.Fatalf("no image in resp: %s", body)
	}
	return out.Data[0].URL
}

// TestImagesGenerationsMioraTextToImage miora 文生图：sync 响应直接出图（无轮询）。
func TestImagesGenerationsMioraTextToImage(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq("POST", "/v1/images/generations",
		`{"model":"cn:miora-image","prompt":"a red apple","size":"1024x1024"}`))
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := decodeImageURL(t, rr.Body.String()); got != "https://cos.example/miora.png" {
		t.Fatalf("url=%q", got)
	}
	// 走的是生图端点，不是改图端点。
	if rec.mioraImageSubmit == "" {
		t.Fatalf("upstream saw no image/generate/submit")
	}
	if rec.mioraImageEditSubmit != "" {
		t.Errorf("text-to-image must not hit edit endpoint: %s", rec.mioraImageEditSubmit)
	}
	if !strings.Contains(rec.mioraImageSubmit, `"prompt":"a red apple"`) ||
		!strings.Contains(rec.mioraImageSubmit, `"size":"1024x1024"`) {
		t.Errorf("submit body=%s", rec.mioraImageSubmit)
	}
	if strings.Contains(rec.mioraImageSubmit, `"images"`) {
		t.Errorf("text-to-image must not carry images: %s", rec.mioraImageSubmit)
	}
	// 与视频同一条鉴权链：先签 ck_t_，媒体端点用 ck_t_。
	if rec.apiKeyAuth != "Bearer at1" || rec.mioraAuth != "Bearer ck_t_test.abc" {
		t.Errorf("auth api-keys=%q miora=%q", rec.apiKeyAuth, rec.mioraAuth)
	}
	// sync 结果不该再去 query（上游会对 sync 任务回 INVALID_TASK_ID_FOR_SYNC_MODE）。
	if rec.mioraQueryRaw != "" {
		t.Errorf("sync image must not be polled: %s", rec.mioraQueryRaw)
	}
}

// TestImagesGenerationsMioraEdit 带 images 即改图：走 edit 端点，data URI 前缀被剥成裸 base64。
func TestImagesGenerationsMioraEdit(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq("POST", "/v1/images/generations",
		`{"model":"cn:miora-image","prompt":"make it blue",`+
			`"images":["data:image/png;base64,QQ==",{"url":"https://x/b.png"}]}`))
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := decodeImageURL(t, rr.Body.String()); got != "https://cos.example/miora-edit.png" {
		t.Fatalf("url=%q", got)
	}
	if rec.mioraImageSubmit != "" {
		t.Errorf("edit must not hit generate endpoint: %s", rec.mioraImageSubmit)
	}
	if !strings.Contains(rec.mioraImageEditSubmit, `"images":[{"base64":"QQ=="},{"url":"https://x/b.png"}]`) {
		t.Errorf("edit body=%s", rec.mioraImageEditSubmit)
	}
}

// TestImagesGenerationsMioraValidation 本地闸门：n>1、非 miora 模型带 images 均在本地 400。
func TestImagesGenerationsMioraValidation(t *testing.T) {
	cases := []string{
		`{"model":"cn:miora-image","prompt":"x","n":2}`,                             // 上游一次只出一张
		`{"model":"hunyuan-image-alpha","prompt":"x","images":["https://x/a.png"]}`, // 旧通道不认 images
		`{"model":"hunyuan-image-alpha","prompt":"x","quality":"hd"}`,               // 旧通道不认 quality
	}
	for _, body := range cases {
		rec := &mediaRecorder{}
		h := mediaHandler(t, rec)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, authedReq("POST", "/v1/images/generations", body))
		if rr.Code != 400 {
			t.Errorf("body=%s code=%d want 400", body, rr.Code)
		}
		if rec.mioraImageSubmit != "" || rec.mioraImageEditSubmit != "" {
			t.Errorf("body=%s must be rejected locally, upstream saw a call", body)
		}
	}
}

// TestImagesEditsCompatMultipart multipart 改图：文件字节转 base64 落到上游 images。
func TestImagesEditsCompatMultipart(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range map[string]string{
		"model": "cn:miora-image-edit", "prompt": "make it blue",
		"n": "1", "response_format": "url", "size": "1024x1024",
	} {
		_ = mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("image", "a.png")
	_, _ = fw.Write([]byte("PNGDATA"))
	_ = mw.Close()

	req := httptest.NewRequest("POST", "/v1/images/edits", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer sk-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := decodeImageURL(t, rr.Body.String()); got != "https://cos.example/miora-edit.png" {
		t.Fatalf("url=%q", got)
	}
	if !strings.Contains(rec.mioraImageEditSubmit, `"base64":"UE5HREFUQQ=="`) {
		t.Errorf("file not forwarded as base64: %s", rec.mioraImageEditSubmit)
	}
	if !strings.Contains(rec.mioraImageEditSubmit, `"size":"1024x1024"`) {
		t.Errorf("size not forwarded: %s", rec.mioraImageEditSubmit)
	}
}

// TestImagesEditsCompatJSON JSON 改图：URL 参考图直接透传；未给模型时默认走 miora 改图。
func TestImagesEditsCompatJSON(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq("POST", "/v1/images/edits",
		`{"prompt":"recolor to blue","images":["https://x/a.png",{"file_key":"bga/abc"}]}`))
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rec.mioraImageEditSubmit, `"images":[{"url":"https://x/a.png"},{"file_key":"bga/abc"}]`) {
		t.Errorf("edit body=%s", rec.mioraImageEditSubmit)
	}
	// 单数 image / image_url 写法也要认。
	rec2 := &mediaRecorder{}
	h2 := mediaHandler(t, rec2)
	rr2 := httptest.NewRecorder()
	h2.ServeHTTP(rr2, authedReq("POST", "/v1/images/edits",
		`{"model":"miora","prompt":"x","image_url":"https://x/c.png"}`))
	if rr2.Code != 200 {
		t.Fatalf("image_url code=%d body=%s", rr2.Code, rr2.Body.String())
	}
	if !strings.Contains(rec2.mioraImageEditSubmit, `"images":[{"url":"https://x/c.png"}]`) {
		t.Errorf("image_url not normalized: %s", rec2.mioraImageEditSubmit)
	}
}

// TestImagesEditsCompatValidation 改图入参闸门：缺图 / 缺 prompt / mask / 非 miora 模型 / n>1。
func TestImagesEditsCompatValidation(t *testing.T) {
	cases := []struct{ name, body string }{
		{"no image", `{"prompt":"x"}`},
		{"empty images", `{"prompt":"x","images":[]}`},
		{"no prompt", `{"images":["https://x/a.png"]}`},
		{"mask unsupported", `{"prompt":"x","images":["https://x/a.png"],"mask":"https://x/m.png"}`},
		{"non-miora model", `{"model":"cn:hunyuan-image-alpha","prompt":"x","images":["https://x/a.png"]}`},
		{"n>1", `{"prompt":"x","images":["https://x/a.png"],"n":2}`},
		{"bad payload", `{"prompt":"x","images":[{"base64":"QQ==","url":"https://x/a.png"}]}`},
	}
	for _, c := range cases {
		rec := &mediaRecorder{}
		h := mediaHandler(t, rec)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, authedReq("POST", "/v1/images/edits", c.body))
		if rr.Code != 400 {
			t.Errorf("%s: code=%d want 400 (body=%s)", c.name, rr.Code, rr.Body.String())
		}
		if rec.mioraImageEditSubmit != "" {
			t.Errorf("%s: must be rejected locally, upstream saw a call", c.name)
		}
	}
}

// TestImagesEditsCompatMultipartNoImage multipart 且没带图 → 400（不发上游）。
func TestImagesEditsCompatMultipartNoImage(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("prompt", "x")
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/v1/images/edits", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer sk-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("code=%d want 400 body=%s", rr.Code, rr.Body.String())
	}
	if rec.mioraImageEditSubmit != "" {
		t.Errorf("upstream saw a call: %s", rec.mioraImageEditSubmit)
	}
}

// TestIsMioraImageSpec 模型规格判定：图片 ID / 裸厂商名命中，视频 ID 不命中。
func TestIsMioraImageSpec(t *testing.T) {
	yes := []string{"miora-image", "miora-image-edit", "Miora-Image", "miora"}
	no := []string{"", "miora-minimax-h3", "hunyuan-image-alpha", "kling-v2.6"}
	for _, s := range yes {
		if !isMioraImageSpec(s) {
			t.Errorf("%q should be a miora image spec", s)
		}
	}
	for _, s := range no {
		if isMioraImageSpec(s) {
			t.Errorf("%q should NOT be a miora image spec", s)
		}
	}
	if !upstream.IsMioraImageModel(upstream.MioraImageEditModel) {
		t.Errorf("catalog ID %q must match the image prefix", upstream.MioraImageEditModel)
	}
}

// TestNormalizeImageSize size 归一：上游只认「数字x数字」，前端爱发比例或 auto。
//
// 实测背景：直接把 `1:1` / `16:9` / `auto` 转给上游会回
// 400 `size must be in format "数字x数字"`（「Nww-无限画布」的图像尺寸选择器就是发比例的）。
func TestNormalizeImageSize(t *testing.T) {
	cases := map[string]string{
		// auto / 空 → 丢弃，让上游用默认
		"":     "",
		"  ":   "",
		"auto": "",
		"AUTO": "",
		// 比例 → 像素（忠实翻译，画幅语义不变）
		"1:1":  "1024x1024",
		"4:3":  "1024x768",
		"3:4":  "768x1024",
		"3:2":  "1152x768",
		"2:3":  "768x1152",
		"16:9": "1280x720",
		"9:16": "720x1280",
		"21:9": "1344x576",
		// 已是像素 → 归一分隔符/空白后透出
		"1024x1024":    "1024x1024",
		"1024X1024":    "1024x1024",
		"1024*1024":    "1024x1024",
		"1024×1024":    "1024x1024",
		"1024 x 1024":  "1024x1024",
		"  512 × 512 ": "512x512",
		// 无法识别 → 原样透出，交上游清晰报错（不静默改写画幅）
		"2K":    "2K",
		"huge":  "huge",
		"1024x": "1024x",
	}
	for in, want := range cases {
		if got := normalizeImageSize(in); got != want {
			t.Errorf("normalizeImageSize(%q) = %q, want %q", in, got, want)
		}
	}
}
