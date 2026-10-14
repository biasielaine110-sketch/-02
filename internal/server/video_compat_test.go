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
)

// multipartVideoReq 构造一次带 input_reference 文件的 multipart 提交（前端「有参考图」的形态）。
func multipartVideoReq(t *testing.T, fields map[string]string, fileContent string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("write field %s: %v", k, err)
		}
	}
	fw, err := mw.CreateFormFile("input_reference", "ref.png")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write([]byte(fileContent)); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	req := httptest.NewRequest("POST", "/v1/videos", &buf)
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// decodeJSONObj 解出响应体为 map。
func decodeJSONObj(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	return out
}

// TestVideosCompatCreateJSONTextToVideo JSON 形态的文生视频：返回 OpenAI 形状 {id,status:queued}，
// 且把秒数/画幅翻译进上游提交体。
func TestVideosCompatCreateJSONTextToVideo(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)

	body := `{"model":"cn:kling-v2.6","prompt":"a dog","seconds":"6","duration":6,"ratio":"16:9","aspect_ratio":"16:9","size":"16:9","resolution":"720p"}`
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq("POST", "/v1/videos", body))
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	out := decodeJSONObj(t, rr)
	if out["id"] != "job-123" {
		t.Fatalf("id=%v body=%s", out["id"], rr.Body.String())
	}
	if out["status"] != "queued" || out["object"] != "video" {
		t.Fatalf("status/object=%v/%v", out["status"], out["object"])
	}
	// Kling 通道只消费时长；秒数从 seconds 别名解析而来（落在 ModelParam 内层 JSON 串里）。
	var kling struct {
		ModelParam string `json:"ModelParam"`
	}
	if err := json.Unmarshal([]byte(rec.submitRaw), &kling); err != nil {
		t.Fatalf("decode kling submit: %v body=%s", err, rec.submitRaw)
	}
	if !strings.Contains(kling.ModelParam, `"Duration":6`) {
		t.Fatalf("kling ModelParam=%s body=%s", kling.ModelParam, rec.submitRaw)
	}
}

// TestVideosCompatCreateJSONMioraSingleFrame input_reference 单张（image_url 对象）→ miora 首帧。
func TestVideosCompatCreateJSONMioraSingleFrame(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)

	body := `{"model":"cn:miora-minimax-h3","prompt":"a cat","duration":"5","aspect_ratio":"3:2","resolution":"720p","input_reference":{"image_url":"https://x.example/1.png"}}`
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq("POST", "/v1/videos", body))
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	var sent struct {
		Duration    int    `json:"duration"`
		Resolution  string `json:"resolution"`
		AspectRatio string `json:"aspect_ratio"`
		Input       struct {
			FirstFrame *struct {
				URL string `json:"url"`
			} `json:"first_frame_image"`
			ReferenceImages []struct{} `json:"reference_images"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(rec.mioraSubmit), &sent); err != nil {
		t.Fatalf("decode miora submit: %v body=%s", err, rec.mioraSubmit)
	}
	if sent.Input.FirstFrame == nil || sent.Input.FirstFrame.URL != "https://x.example/1.png" {
		t.Fatalf("first_frame_image=%+v body=%s", sent.Input.FirstFrame, rec.mioraSubmit)
	}
	if len(sent.Input.ReferenceImages) != 0 {
		t.Fatalf("unexpected reference_images: %s", rec.mioraSubmit)
	}
	// 别名归一：720p→768P、3:2→4:3。
	if sent.Resolution != "768P" || sent.AspectRatio != "4:3" || sent.Duration != 5 {
		t.Fatalf("normalized duration/resolution/aspect=%d/%s/%s body=%s",
			sent.Duration, sent.Resolution, sent.AspectRatio, rec.mioraSubmit)
	}
}

// TestVideosCompatCreateJSONMioraMultiReference input_reference 多张 → miora 多参考。
func TestVideosCompatCreateJSONMioraMultiReference(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)

	body := `{"model":"miora-minimax-h3","prompt":"a bird","input_reference":["https://x.example/1.png","https://x.example/2.png"]}`
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq("POST", "/v1/videos", body))
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	var sent struct {
		Input struct {
			FirstFrame      json.RawMessage  `json:"first_frame_image"`
			ReferenceImages []map[string]any `json:"reference_images"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(rec.mioraSubmit), &sent); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.mioraSubmit)
	}
	if len(sent.Input.ReferenceImages) != 2 {
		t.Fatalf("reference_images=%v body=%s", sent.Input.ReferenceImages, rec.mioraSubmit)
	}
	if sent.Input.ReferenceImages[1]["url"] != "https://x.example/2.png" {
		t.Fatalf("ref[1]=%v", sent.Input.ReferenceImages[1])
	}
	if len(sent.Input.FirstFrame) != 0 {
		t.Fatalf("unexpected first_frame: %s", sent.Input.FirstFrame)
	}
}

// TestVideosCompatCreateMultipart multipart 形态：input_reference 文件 → miora 首帧 base64。
func TestVideosCompatCreateMultipart(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)

	req := multipartVideoReq(t, map[string]string{
		"model":         "cn:miora-minimax-h3",
		"prompt":        "a robot",
		"seconds":       "8",
		"duration":      "8",
		"ratio":         "9:16",
		"aspect_ratio":  "9:16",
		"resolution":    "1080p",
		"watermark":     "false",
		"generate_audio": "true",
	}, "PNGDATA")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if id := decodeJSONObj(t, rr)["id"]; id != "miora-task-1" {
		t.Fatalf("id=%v body=%s", id, rr.Body.String())
	}
	var sent struct {
		Duration   int    `json:"duration"`
		Resolution string `json:"resolution"`
		Input      struct {
			FirstFrame *struct {
				Base64 string `json:"base64"`
			} `json:"first_frame_image"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(rec.mioraSubmit), &sent); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.mioraSubmit)
	}
	if sent.Input.FirstFrame == nil {
		t.Fatalf("missing first_frame_image body=%s", rec.mioraSubmit)
	}
	if want := base64.StdEncoding.EncodeToString([]byte("PNGDATA")); sent.Input.FirstFrame.Base64 != want {
		t.Fatalf("base64=%q want %q", sent.Input.FirstFrame.Base64, want)
	}
	if sent.Resolution != "2K" || sent.Duration != 8 {
		t.Fatalf("normalized resolution/duration=%s/%d body=%s", sent.Resolution, sent.Duration, rec.mioraSubmit)
	}
}

// TestVideosCompatKlingRejectsReferences 参考图只对 miora 生效；Kling 携带时本地 400，不打上游。
func TestVideosCompatKlingRejectsReferences(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)

	body := `{"model":"cn:kling-v2.6","prompt":"a dog","input_reference":"https://x.example/1.png"}`
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq("POST", "/v1/videos", body))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if rec.submitRaw != "" {
		t.Fatalf("upstream should not be called, got %s", rec.submitRaw)
	}
}

// TestVideosCompatPromptRequired 缺 prompt → 400，不打上游。
func TestVideosCompatPromptRequired(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq("POST", "/v1/videos", `{"model":"cn:kling-v2.6"}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if rec.submitRaw != "" {
		t.Fatalf("upstream should not be called")
	}
}

// TestVideosCompatGetStatus 轮询：完成后给出 status=completed 与 video_url/url 直链。
func TestVideosCompatGetStatus(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)

	// 先提交（miora），任务才会登记进任务表。
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq("POST", "/v1/videos", `{"model":"cn:miora-minimax-h3","prompt":"a cat"}`))
	if rr.Code != 200 {
		t.Fatalf("submit code=%d body=%s", rr.Code, rr.Body.String())
	}
	jobID := decodeJSONObj(t, rr)["id"].(string)

	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, authedReq("GET", "/v1/videos/"+jobID, ""))
	if rr2.Code != 200 {
		t.Fatalf("get code=%d body=%s", rr2.Code, rr2.Body.String())
	}
	out := decodeJSONObj(t, rr2)
	if out["status"] != "completed" {
		t.Fatalf("status=%v body=%s", out["status"], rr2.Body.String())
	}
	if out["video_url"] != "https://cos.example/miora.mp4" || out["url"] != "https://cos.example/miora.mp4" {
		t.Fatalf("video_url/url=%v/%v", out["video_url"], out["url"])
	}
}

// TestVideosCompatGetUnknownJob 未知任务 → 404（提示重新提交），不编造状态。
func TestVideosCompatGetUnknownJob(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq("GET", "/v1/videos/nope", ""))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
}

// TestVideosCompatNativeEndpointsIntact 兼容层不得影响原生端点路由。
func TestVideosCompatNativeEndpointsIntact(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq("POST", "/v1/videos/generations", `{"model":"cn:kling-v2.6","prompt":"x"}`))
	if rr.Code != 200 || decodeJSONObj(t, rr)["status"] != "submitted" {
		t.Fatalf("native submit code=%d body=%s", rr.Code, rr.Body.String())
	}

	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, authedReq("GET", "/v1/videos/generations/job-123", ""))
	if rr2.Code != 200 || decodeJSONObj(t, rr2)["status"] != "succeeded" {
		t.Fatalf("native get code=%d body=%s", rr2.Code, rr2.Body.String())
	}
}

// TestVideoCompatJSONContentTypeWithCharset Content-Type 带 charset 也要走 JSON 分支。
func TestVideoCompatJSONContentTypeWithCharset(t *testing.T) {
	rec := &mediaRecorder{}
	h := mediaHandler(t, rec)
	req := httptest.NewRequest("POST", "/v1/videos", strings.NewReader(`{"model":"cn:kling-v2.6","prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
}

// TestVideoCompatHelpers 归一化小工具：分辨率别名、画幅别名、size 判别、秒数宽松解析。
func TestVideoCompatHelpers(t *testing.T) {
	cases := []struct{ in, want string }{
		{"720p", "768P"}, {"720", "768P"}, {"768p", "768P"},
		{"1080p", "2K"}, {"2K", "2K"}, {"4k", "2K"}, {"", ""}, {"odd", "odd"},
	}
	for _, c := range cases {
		if got := normalizeVideoResolution(c.in); got != c.want {
			t.Errorf("normalizeVideoResolution(%q)=%q want %q", c.in, got, c.want)
		}
	}
	arCases := []struct{ in, want string }{
		{"16:9", "16:9"}, {"3:2", "4:3"}, {"2:3", "3:4"}, {"4:5", "3:4"}, {"16:10", "16:9"}, {"", ""},
	}
	for _, c := range arCases {
		if got := normalizeAspectRatio(c.in); got != c.want {
			t.Errorf("normalizeAspectRatio(%q)=%q want %q", c.in, got, c.want)
		}
	}
	if ratioLike("1280x720") != "" || ratioLike(" 16:9 ") != "16:9" {
		t.Errorf("ratioLike misjudged pixel size vs ratio")
	}
	var c compatVideoJSON
	if err := json.Unmarshal([]byte(`{"seconds":"6","duration":7}`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if int(c.Seconds) != 6 || int(c.Duration) != 7 {
		t.Fatalf("looseSeconds seconds/duration=%d/%d", c.Seconds, c.Duration)
	}
}

// TestParseInputReference 空/单张/多张/字符串/base64 各形态。
func TestParseInputReference(t *testing.T) {
	if got, err := parseInputReference(nil); err != nil || len(got) != 0 {
		t.Fatalf("nil: got=%v err=%v", got, err)
	}
	if got, _ := parseInputReference(json.RawMessage(`null`)); len(got) != 0 {
		t.Fatalf("null: got=%v", got)
	}
	got, err := parseInputReference(json.RawMessage(`"data:image/png;base64,QQ=="`))
	if err != nil || len(got) != 1 || got[0].Base64 != "data:image/png;base64,QQ==" {
		t.Fatalf("data uri string: got=%+v err=%v", got, err)
	}
	got, err = parseInputReference(json.RawMessage(`{"url":"https://x/1.png"}`))
	if err != nil || len(got) != 1 || got[0].URL != "https://x/1.png" {
		t.Fatalf("url object: got=%+v err=%v", got, err)
	}
	if got, _ = parseInputReference(json.RawMessage(`[]`)); len(got) != 0 {
		t.Fatalf("empty array: got=%v", got)
	}
}
