package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func mioraTestClient(t *testing.T, submitBody, queryBody string, rec *struct{ submit, query, apiKey string }) *Client {
	t.Helper()
	c := testClient(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/v2/api-keys"):
			if rec != nil {
				rec.apiKey = string(raw)
			}
			return jsonResp(200, `{"code":0,"msg":"OK","data":{"key":"ck_t_abc.def","expires_at":"2026-10-13T23:00:00+08:00"}}`), nil
		case strings.HasSuffix(r.URL.Path, "/video/submit"):
			if rec != nil {
				rec.submit = string(raw)
			}
			return jsonResp(200, submitBody), nil
		case strings.HasSuffix(r.URL.Path, "/video/query-task"):
			if rec != nil {
				rec.query = string(raw)
			}
			return jsonResp(200, queryBody), nil
		}
		return nil, errors.New("unexpected path: " + r.URL.Path)
	})
	c.MioraBase = "https://miora.example"
	return c
}

func TestSubmitVideoMiora(t *testing.T) {
	rec := &struct{ submit, query, apiKey string }{}
	c := mioraTestClient(t,
		`{"code":0,"failed":false,"data":{"upstreamTaskId":"1312-WandVideo-abc","model":"minimax-h3-iOA","provider":"copilot-video"},"message":"successful","httpStatus":200}`,
		"", rec)
	a := &auth.Auth{UID: "u1", AccessToken: "at1"}

	id, err := c.SubmitVideoMiora(context.Background(), a, MioraVideoRequest{
		Prompt: "a cat on a skateboard", DurationSec: 6, Resolution: "768p", AspectRatio: "16:9",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if id != "1312-WandVideo-abc" {
		t.Fatalf("id=%q", id)
	}
	// 时长/resolution(归一为大写)/aspect_ratio 三项都落到请求体。
	for _, want := range []string{`"prompt":"a cat on a skateboard"`, `"duration":6`, `"resolution":"768P"`, `"aspect_ratio":"16:9"`} {
		if !strings.Contains(rec.submit, want) {
			t.Errorf("submit body %s missing %s", rec.submit, want)
		}
	}
	// 文生模式不得携带 input（带上会被上游按图生视频解析）。
	if strings.Contains(rec.submit, `"input"`) {
		t.Errorf("text-mode submit body must not carry input: %s", rec.submit)
	}
	// ck_t_ 签发用的是 accessToken。
	if !strings.Contains(rec.apiKey, `"temporary":true`) || !strings.Contains(rec.apiKey, `"name":"workbuddy-connect-cloud-service"`) {
		t.Errorf("api-keys body=%s", rec.apiKey)
	}
}

func TestSubmitVideoMioraValidation(t *testing.T) {
	c := mioraTestClient(t, `{"code":0,"failed":false,"data":{"upstreamTaskId":"x"}}`, "", nil)
	a := &auth.Auth{UID: "u1", AccessToken: "at1"}
	ctx := context.Background()

	cases := []struct {
		name                       string
		prompt, resolution, aspect string
		duration                   int
	}{
		{"empty prompt", "  ", "", "", 6},
		{"duration low", "p", "", "", 3},
		{"duration high", "p", "", "", 16},
		{"bad resolution", "p", "1080p", "", 6},
		{"bad aspect", "p", "", "5:4", 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.SubmitVideoMiora(ctx, a, MioraVideoRequest{
				Prompt: tc.prompt, DurationSec: tc.duration, Resolution: tc.resolution, AspectRatio: tc.aspect,
			})
			var uerr *Error
			if !errors.As(err, &uerr) || uerr.Kind != ErrBadParams {
				t.Fatalf("err=%v want ErrBadParams", err)
			}
		})
	}
}

func TestQueryVideoMioraStatusMapping(t *testing.T) {
	cases := []struct {
		name       string
		queryBody  string
		wantStatus string
		wantURL    string
		wantFail   bool
	}{
		{
			name:       "completed",
			queryBody:  `{"code":0,"failed":false,"data":{"t1":{"status":"completed","resultFiles":[{"signedUrl":"https://cos.example/v.mp4","mimeType":"video/mp4"}]}}}`,
			wantStatus: "DONE",
			wantURL:    "https://cos.example/v.mp4",
		},
		{
			name:       "processing",
			queryBody:  `{"code":0,"failed":false,"data":{"t1":{"status":"processing"}}}`,
			wantStatus: "RUNNING",
		},
		{
			name:       "pending with harmless errorMessage stays running",
			queryBody:  `{"code":0,"failed":false,"data":{"t1":{"status":"pending","errorMessage":"Request failed with status code 400"}}}`,
			wantStatus: "RUNNING",
		},
		{
			name:       "failed",
			queryBody:  `{"code":0,"failed":false,"data":{"t1":{"status":"failed","errorMessage":"content safety"}}}`,
			wantStatus: "FAIL",
			wantFail:   true,
		},
		{
			name:       "completed without file is failure",
			queryBody:  `{"code":0,"failed":false,"data":{"t1":{"status":"completed"}}}`,
			wantStatus: "FAIL",
			wantFail:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mioraTestClient(t, "", tc.queryBody, nil)
			a := &auth.Auth{UID: "u1", AccessToken: "at1"}
			job, err := c.QueryVideoMiora(context.Background(), a, "t1")
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			if job.Status != tc.wantStatus {
				t.Errorf("status=%q want %q", job.Status, tc.wantStatus)
			}
			if job.ResultURL != tc.wantURL {
				t.Errorf("url=%q want %q", job.ResultURL, tc.wantURL)
			}
			if job.Failed() != tc.wantFail {
				t.Errorf("Failed()=%v want %v", job.Failed(), tc.wantFail)
			}
		})
	}
}

// TestMioraEnvelopeErrorMatchesCode 覆盖两类失败信封：
// 业务失败（failed=true + code 非零数值）与鉴权失败（无 failed 字段 + 字符串 code）。
func TestMioraEnvelopeErrorMatchesCode(t *testing.T) {
	cases := []struct {
		name string
		body string
		kind ErrKind
	}{
		{"business fail (prompt missing)", `{"code":"UPSTREAM_SUBMIT_FAILED","failed":true,"data":null,"message":"upstream video-submit failed: status=400 code=10001 msg=Create video failed with error: TokenHub MiniMax H3 requires prompt","httpStatus":502}`, ErrBadParams},
		{"auth fail string code", `{"code":"AUTH_MISSING_TEMP_KEY","data":null,"message":"缺少 Authorization Bearer","httpStatus":401}`, ErrAccountFault},
		{"content blocked", `{"code":"CONTENT_SAFETY","failed":true,"data":null,"message":"content safety rejected","httpStatus":400}`, ErrContentBlocked},
		// 媒体校验错误：code=10005 + message 是**字符串数组**（实测上游形态），必须能解析且归为参数错误。
		{"validation array message", `{"code":10005,"failed":true,"data":null,"message":["input.first_frame_image.MediaPayload requires exactly one of: base64, url, file_key"],"httpStatus":400}`, ErrBadParams},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mioraTestClient(t, tc.body, "", nil)
			a := &auth.Auth{UID: "u1", AccessToken: "at1"}
			_, err := c.SubmitVideoMiora(context.Background(), a, MioraVideoRequest{Prompt: "a prompt"})
			var uerr *Error
			if !errors.As(err, &uerr) {
				t.Fatalf("err=%v want *Error", err)
			}
			if uerr.Kind != tc.kind {
				t.Errorf("kind=%v want %v (msg=%s)", uerr.Kind, tc.kind, uerr.Msg)
			}
		})
	}
}

// TestSignClientTempKeyRejectsNonCKTPrefix 签发结果不以 ck_t_ 开头即视为失败。
func TestSignClientTempKeyRejectsNonCKTPrefix(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"msg":"OK","data":{"key":"tk_wrongprefix"}}`), nil
	})
	_, err := c.signClientTempKey(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at1"})
	var uerr *Error
	if !errors.As(err, &uerr) || uerr.Kind != ErrAccountFault {
		t.Fatalf("err=%v want ErrAccountFault", err)
	}
}

func TestMioraVideoStatus(t *testing.T) {
	cases := map[string]string{
		"completed": "DONE", "COMPLETED": "DONE", "succeeded": "DONE",
		"failed": "FAIL", "error": "FAIL", "canceled": "FAIL",
		"pending": "RUNNING", "processing": "RUNNING", "running": "RUNNING",
		"": "RUNNING", "whatever": "RUNNING",
	}
	for in, want := range cases {
		if got := mioraVideoStatus(in); got != want {
			t.Errorf("mioraVideoStatus(%q)=%q want %q", in, got, want)
		}
	}
}

// TestMediaPayloadUnmarshal 字符串简写（URL / data URI / 裸 base64）与对象形态都要能解。
func TestMediaPayloadUnmarshal(t *testing.T) {
	cases := []struct {
		in   string
		want MediaPayload
	}{
		{`"https://x/a.png"`, MediaPayload{URL: "https://x/a.png"}},
		{`"http://x/a.png"`, MediaPayload{URL: "http://x/a.png"}},
		{`"data:image/png;base64,QQ=="`, MediaPayload{Base64: "data:image/png;base64,QQ=="}},
		{`"QUJD"`, MediaPayload{Base64: "QUJD"}},
		{`{"url":"https://x/b.png"}`, MediaPayload{URL: "https://x/b.png"}},
		{`{"base64":"ZZZ"}`, MediaPayload{Base64: "ZZZ"}},
		{`{"file_key":"bga/xyz"}`, MediaPayload{FileKey: "bga/xyz"}},
	}
	for _, tc := range cases {
		var p MediaPayload
		if err := json.Unmarshal([]byte(tc.in), &p); err != nil {
			t.Fatalf("unmarshal %s: %v", tc.in, err)
		}
		if p != tc.want {
			t.Errorf("unmarshal %s = %+v want %+v", tc.in, p, tc.want)
		}
	}
}

// TestMediaPayloadNormalized data URI 剥离 + 「恰含一个字段」校验。
func TestMediaPayloadNormalized(t *testing.T) {
	got, err := (MediaPayload{Base64: "data:image/png;base64,QUJD"}).normalized()
	if err != nil {
		t.Fatalf("normalized: %v", err)
	}
	if got.Base64 != "QUJD" || got.URL != "" || got.FileKey != "" {
		t.Fatalf("data URI not stripped: %+v", got)
	}
	for _, bad := range []MediaPayload{{}, {Base64: "a", URL: "b"}, {Base64: "  "}, {URL: "u", FileKey: "f"}} {
		if _, err := bad.normalized(); err == nil {
			t.Errorf("payload %+v should be rejected", bad)
		}
	}
}

// TestMioraBuildInput 三种模式的 input 构造与本地强校验（防「空 input 静默降级成文生」）。
func TestMioraBuildInput(t *testing.T) {
	first := MediaPayload{URL: "https://x/a.png"}
	last := MediaPayload{Base64: "data:image/png;base64,QQ=="}
	refs := []MediaPayload{{URL: "https://x/1.png"}, {URL: "https://x/2.png"}}
	empty := []MediaPayload{}

	cases := []struct {
		name    string
		req     MioraVideoRequest
		wantKey string // 期望 input 含该键；空串表示应为文生（input == nil）
		wantErr bool
	}{
		{"text mode (no input)", MioraVideoRequest{Prompt: "p"}, "", false},
		{"frame first only", MioraVideoRequest{Prompt: "p", FirstFrame: &first}, "first_frame_image", false},
		{"frame first+last", MioraVideoRequest{Prompt: "p", FirstFrame: &first, LastFrame: &last}, "last_frame_image", false},
		{"frame last only lacks first", MioraVideoRequest{Prompt: "p", LastFrame: &last}, "", true},
		{"frame empty payload", MioraVideoRequest{Prompt: "p", FirstFrame: &MediaPayload{}}, "", true},
		{"reference ok", MioraVideoRequest{Prompt: "p", ReferenceImages: &refs}, "reference_images", false},
		{"reference empty slice", MioraVideoRequest{Prompt: "p", ReferenceImages: &empty}, "", true},
		{"reference with empty item", MioraVideoRequest{Prompt: "p", ReferenceImages: &[]MediaPayload{{}}}, "", true},
		{"frame + reference conflict", MioraVideoRequest{Prompt: "p", FirstFrame: &first, ReferenceImages: &refs}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input, err := tc.req.buildInput()
			if tc.wantErr {
				var uerr *Error
				if !errors.As(err, &uerr) || uerr.Kind != ErrBadParams {
					t.Fatalf("err=%v want ErrBadParams", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if tc.wantKey == "" {
				if input != nil {
					t.Fatalf("want nil input (text mode), got %v", input)
				}
				return
			}
			if _, ok := input[tc.wantKey]; !ok {
				t.Fatalf("input %v missing key %q", input, tc.wantKey)
			}
		})
	}
}

// TestSubmitVideoMioraFrameBody 首尾帧模式：input.first_frame_image / last_frame_image 落到请求体。
func TestSubmitVideoMioraFrameBody(t *testing.T) {
	rec := &struct{ submit, query, apiKey string }{}
	c := mioraTestClient(t, `{"code":0,"failed":false,"data":{"upstreamTaskId":"t-fr"}}`, "", rec)
	a := &auth.Auth{UID: "u1", AccessToken: "at1"}
	first := MediaPayload{URL: "https://x/first.png"}
	last := MediaPayload{Base64: "data:image/jpeg;base64,ZZZ"}
	id, err := c.SubmitVideoMiora(context.Background(), a, MioraVideoRequest{
		Prompt: "morph", DurationSec: 5, FirstFrame: &first, LastFrame: &last,
	})
	if err != nil || id != "t-fr" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	for _, want := range []string{
		`"input":{`,
		`"first_frame_image":{"url":"https://x/first.png"}`,
		`"last_frame_image":{"base64":"ZZZ"}`,
	} {
		if !strings.Contains(rec.submit, want) {
			t.Errorf("submit body %s missing %s", rec.submit, want)
		}
	}
}

// TestSubmitVideoMioraReferenceBody 参考生模式：input.reference_images 数组落到请求体。
func TestSubmitVideoMioraReferenceBody(t *testing.T) {
	rec := &struct{ submit, query, apiKey string }{}
	c := mioraTestClient(t, `{"code":0,"failed":false,"data":{"upstreamTaskId":"t-ref"}}`, "", rec)
	a := &auth.Auth{UID: "u1", AccessToken: "at1"}
	refs := []MediaPayload{{URL: "https://x/1.png"}, {URL: "https://x/2.png"}}
	id, err := c.SubmitVideoMiora(context.Background(), a, MioraVideoRequest{Prompt: "x", DurationSec: 6, ReferenceImages: &refs})
	if err != nil || id != "t-ref" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if !strings.Contains(rec.submit, `"reference_images":[{"url":"https://x/1.png"},{"url":"https://x/2.png"}]`) {
		t.Errorf("submit body=%s", rec.submit)
	}
}

// TestMioraMessageText message 字符串/数组两种形态都要能压成文本。
func TestMioraMessageText(t *testing.T) {
	cases := map[string]string{
		`"boom"`:       "boom",
		`["a","b"]`:    "a; b",
		`["single"]`:   "single",
		`"  padded  "`: "padded",
		``:             "",
	}
	for in, want := range cases {
		if got := mioraMessageText(json.RawMessage(in)); got != want {
			t.Errorf("mioraMessageText(%s)=%q want %q", in, got, want)
		}
	}
}
