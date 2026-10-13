package upstream

import (
	"context"
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

	id, err := c.SubmitVideoMiora(context.Background(), a, "a cat on a skateboard", "", 6, "768p", "16:9")
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
			_, err := c.SubmitVideoMiora(ctx, a, tc.prompt, "", tc.duration, tc.resolution, tc.aspect)
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mioraTestClient(t, tc.body, "", nil)
			a := &auth.Auth{UID: "u1", AccessToken: "at1"}
			_, err := c.SubmitVideoMiora(context.Background(), a, "a prompt", "", 0, "", "")
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
