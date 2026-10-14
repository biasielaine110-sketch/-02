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

// mioraImageRec 记录图片通道收到的请求体。
type mioraImageRec struct {
	generate string
	edit     string
	query    string
	apiKey   string
}

// mioraImageClient 假上游：签发 ck_t_ + 图片三端点。
// queryBodies 按顺序供多次 query 使用（用尽后重复最后一条）。
func mioraImageClient(generateBody, editBody string, queryBodies []string, rec *mioraImageRec) *Client {
	qi := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/v2/api-keys"):
			if rec != nil {
				rec.apiKey = string(raw)
			}
			return jsonResp(200, `{"code":0,"msg":"OK","data":{"key":"ck_t_abc.def"}}`), nil
		case strings.HasSuffix(r.URL.Path, "/image/generate/submit"):
			if rec != nil {
				rec.generate = string(raw)
			}
			return jsonResp(200, generateBody), nil
		case strings.HasSuffix(r.URL.Path, "/image/edit/submit"):
			if rec != nil {
				rec.edit = string(raw)
			}
			return jsonResp(200, editBody), nil
		case strings.HasSuffix(r.URL.Path, "/image/query-task"):
			if rec != nil {
				rec.query = string(raw)
			}
			body := ""
			if len(queryBodies) > 0 {
				body = queryBodies[qi]
				if qi < len(queryBodies)-1 {
					qi++
				}
			}
			return jsonResp(200, body), nil
		}
		return nil, errors.New("unexpected path: " + r.URL.Path)
	})
	c.MioraBase = "https://miora.example"
	return c
}

const imageSyncOK = `{"code":0,"failed":false,"message":"successful","httpStatus":200,` +
	`"data":{"upstreamTaskId":"wb-sync-1","mode":"sync","status":"completed",` +
	`"model":"hy-image-3.5-wb","provider":"copilot-image","errorMessage":null,` +
	`"resultFiles":[{"signedUrl":"https://cos.example/i.png","mimeType":"image/png","expiresAt":1791952510}]}}`

// TestSubmitImageMioraTextToImage 文生图：sync 结果直接返回，且**不去 query**。
func TestSubmitImageMioraTextToImage(t *testing.T) {
	rec := &mioraImageRec{}
	c := mioraImageClient(imageSyncOK, "", nil, rec)
	a := &auth.Auth{UID: "u1", AccessToken: "at1"}

	res, err := c.SubmitImageMiora(context.Background(), a, MioraImageRequest{
		Prompt: "a red apple", Size: "1024x1024", Quality: "hd",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.Status != "DONE" || res.URL != "https://cos.example/i.png" {
		t.Fatalf("res=%+v", res)
	}
	if res.Model != "hy-image-3.5-wb" || res.MimeType != "image/png" || res.ExpiresAt != 1791952510 {
		t.Fatalf("res meta=%+v", res)
	}
	for _, want := range []string{`"prompt":"a red apple"`, `"size":"1024x1024"`, `"quality":"hd"`} {
		if !strings.Contains(rec.generate, want) {
			t.Errorf("generate body %s missing %s", rec.generate, want)
		}
	}
	if strings.Contains(rec.generate, `"images"`) {
		t.Errorf("text-to-image must not carry images: %s", rec.generate)
	}
	if rec.query != "" {
		t.Errorf("sync task must not be polled: %s", rec.query)
	}
	if rec.edit != "" {
		t.Errorf("text-to-image hit edit endpoint: %s", rec.edit)
	}
}

// TestSubmitImageMioraEdit 改图：走 edit 端点，载荷归一（剥 data URI 前缀）。
func TestSubmitImageMioraEdit(t *testing.T) {
	rec := &mioraImageRec{}
	c := mioraImageClient("", strings.Replace(imageSyncOK, "hy-image-3.5-wb", "hy-image-3.5-edit-wb", 1), nil, rec)
	a := &auth.Auth{UID: "u1", AccessToken: "at1"}

	imgs := []MediaPayload{
		{Base64: "data:image/png;base64,QQ=="},
		{URL: "https://x/b.png"},
		{FileKey: "bga/abc"},
	}
	res, err := c.SubmitImageMiora(context.Background(), a, MioraImageRequest{Prompt: "make it blue", Images: &imgs})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.Status != "DONE" || res.Model != "hy-image-3.5-edit-wb" {
		t.Fatalf("res=%+v", res)
	}
	if !strings.Contains(rec.edit, `"images":[{"base64":"QQ=="},{"url":"https://x/b.png"},{"file_key":"bga/abc"}]`) {
		t.Errorf("edit body=%s", rec.edit)
	}
	if rec.generate != "" {
		t.Errorf("edit hit generate endpoint: %s", rec.generate)
	}
}

// TestSubmitImageMioraValidation 本地闸门：空 prompt / 空 images / 非法载荷都不发上游。
func TestSubmitImageMioraValidation(t *testing.T) {
	rec := &mioraImageRec{}
	c := mioraImageClient(imageSyncOK, imageSyncOK, nil, rec)
	a := &auth.Auth{UID: "u1", AccessToken: "at1"}
	ctx := context.Background()

	empty := []MediaPayload{}
	both := []MediaPayload{{Base64: "QQ==", URL: "https://x/a.png"}}

	cases := []struct {
		name string
		req  MioraImageRequest
	}{
		{"empty prompt", MioraImageRequest{Prompt: "   "}},
		{"empty images", MioraImageRequest{Prompt: "p", Images: &empty}},
		{"payload with two fields", MioraImageRequest{Prompt: "p", Images: &both}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.SubmitImageMiora(ctx, a, tc.req)
			var uerr *Error
			if !errors.As(err, &uerr) || uerr.Kind != ErrBadParams {
				t.Fatalf("err=%v want ErrBadParams", err)
			}
		})
	}
	if rec.generate != "" || rec.edit != "" {
		t.Errorf("nothing should reach upstream, generate=%s edit=%s", rec.generate, rec.edit)
	}
}

// TestSubmitImageMioraAsyncPolls 非 sync 且未终态 → 回退轮询 query-task 直到终态。
func TestSubmitImageMioraAsyncPolls(t *testing.T) {
	submit := `{"code":0,"failed":false,"message":"successful","httpStatus":200,` +
		`"data":{"upstreamTaskId":"t-async","mode":"async","status":"pending"}}`
	processing := `{"code":0,"failed":false,"data":{"t-async":{"status":"processing"}},"message":"ok","httpStatus":200}`
	done := `{"code":0,"failed":false,"data":{"t-async":{"status":"completed","resultFiles":[{"signedUrl":"https://cos.example/async.png","mimeType":"image/png"}]}},"message":"ok","httpStatus":200}`
	rec := &mioraImageRec{}
	c := mioraImageClient(submit, "", []string{processing, done}, rec)

	res, err := c.SubmitImageMiora(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at1"},
		MioraImageRequest{Prompt: "p"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.Status != "DONE" || res.URL != "https://cos.example/async.png" {
		t.Fatalf("res=%+v", res)
	}
	if !strings.Contains(rec.query, `"upstreamTaskIds":["t-async"]`) {
		t.Errorf("query body=%s", rec.query)
	}
}

// TestSubmitImageMioraSyncFailure sync 但上游报 failed → 带原文的错误。
func TestSubmitImageMioraSyncFailure(t *testing.T) {
	failed := `{"code":0,"failed":false,"message":"successful","httpStatus":200,` +
		`"data":{"upstreamTaskId":"wb-sync-9","mode":"sync","status":"failed","errorMessage":"CONTENT_SAFETY: blocked"}}`
	c := mioraImageClient(failed, "", nil, &mioraImageRec{})

	_, err := c.SubmitImageMiora(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at1"},
		MioraImageRequest{Prompt: "p"})
	var uerr *Error
	if !errors.As(err, &uerr) {
		t.Fatalf("err=%v want *Error", err)
	}
	if !strings.Contains(uerr.Msg, "CONTENT_SAFETY") || uerr.Kind != ErrContentBlocked {
		t.Fatalf("err kind=%v msg=%q", uerr.Kind, uerr.Msg)
	}
}

// TestSubmitImageMioraCompletedWithoutFile completed 但没产物 → 视为失败。
func TestSubmitImageMioraCompletedWithoutFile(t *testing.T) {
	noFile := `{"code":0,"failed":false,"message":"successful","httpStatus":200,` +
		`"data":{"upstreamTaskId":"wb-sync-8","mode":"sync","status":"completed","resultFiles":[]}}`
	c := mioraImageClient(noFile, "", nil, &mioraImageRec{})

	_, err := c.SubmitImageMiora(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at1"},
		MioraImageRequest{Prompt: "p"})
	var uerr *Error
	if !errors.As(err, &uerr) || uerr.Kind != ErrServer {
		t.Fatalf("err=%v want ErrServer", err)
	}
}

// TestIsMioraImageModel 图片前缀判定：目录 ID 命中，视频 ID 不命中。
func TestIsMioraImageModel(t *testing.T) {
	yes := []string{"miora-image", "miora-image-edit", "Miora-Image", " miora-image "}
	no := []string{"", "miora-minimax-h3", "miora", "hunyuan-image-alpha"}
	for _, s := range yes {
		if !IsMioraImageModel(s) {
			t.Errorf("%q should match image prefix", s)
		}
	}
	for _, s := range no {
		if IsMioraImageModel(s) {
			t.Errorf("%q should NOT match image prefix", s)
		}
	}
}
