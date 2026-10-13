// media_miora.go miora（MiniMax H3）生视频通道——与 tcproxy（Kling）并列的第二条视频后端。
//
// 机制与 tcproxy **完全不同**，不要混用：
//   - 鉴权：先用账号 accessToken 换一枚临时 client 密钥（POST {chatBase}/v2/api-keys，
//     响应 data.key 以 "ck_t_" 开头，900s 有效），再以 Bearer <ck_t_> 调媒体端点。
//   - 端点：mioraBase + /api/ai/workbuddy-proxy（mioraBase 默认 https://public.miora.qq.com，
//     与 ck_t_ 签发域**分离**）。
//   - 提交 POST /video/submit 表单 {prompt,duration,resolution,aspect_ratio}
//     → data {upstreamTaskId, model:"minimax-h3-iOA", provider:"copilot-video"}；
//     轮询 POST /video/query-task 表单 {upstreamTaskIds:[id]}
//     → data[taskId] = {status, resultFiles:[{signedUrl,mimeType,expiresAt}]}。
//
// 契约来源：官方 miora-mcp dist/cli.cjs（MIORA_API_BASE / VIDEO_SUBMIT_PATH /
// VIDEO_QUERY_PATH、fillVideoCommonFields、pollUntilTerminal）与 app.asar 内
// connect_cloud_service 实现（CLIENT_TEMPKEY_API_PATH="/v2/api-keys"、
// CLIENT_TEMPKEY_AUDIT_NAME、TTL=900s、校验前缀 ck_t_）。
// 本地实测：签 ck_t_ 成功、提交返回 upstreamTaskId、轮询 status=completed 并产出 mp4
// （见 .workbuddy/memory/2026-10-13.md）。
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	// mioraBaseDefault miora 媒体代理默认基址（官方 miora-mcp 的 DEV_MIORA_ENDPOINT_FALLBACK）。
	mioraBaseDefault = "https://public.miora.qq.com"
	// mioraProxyBase miora 代理路径前缀。
	mioraProxyBase = "/api/ai/workbuddy-proxy"
	// mioraVideoSubmitPath / mioraVideoQueryPath 视频提交与轮询端点。
	mioraVideoSubmitPath = mioraProxyBase + "/video/submit"
	mioraVideoQueryPath  = mioraProxyBase + "/video/query-task"
	// mioraAPIKeysPath ck_t_ 临时密钥签发路径（挂在 chatBase 上，与媒体域分离）。
	mioraAPIKeysPath = "/v2/api-keys"

	// MioraVideoVendor miora 通道的厂商名（供 vendor 路由与目录 ID 前缀）。
	MioraVideoVendor = "Miora"
	// MioraVideoModel miora 视频目录 ID；上游实际回报 model=minimax-h3-iOA。
	MioraVideoModel = "minimax-h3"

	// mioraTempKeyName 签发 ck_t_ 时的审计名（app.asar 逐字）。
	mioraTempKeyName = "workbuddy-connect-cloud-service"
	// mioraTempKeyTTLSeconds ck_t_ 有效期（秒）。
	mioraTempKeyTTLSeconds = 900
	// mioraTempKeyPrefix ck_t_ 前缀（用于校验签发结果）。
	mioraTempKeyPrefix = "ck_t_"

	// mioraDurationMin / mioraDurationMax miora H3 时长闸门（实测上游 14407）。
	mioraDurationMin = 4
	mioraDurationMax = 15
)

// mioraResolutions / mioraAspectRatios miora H3 分辨率与画幅白名单（实测上游 14407 错误原文）。
var (
	mioraResolutions  = map[string]struct{}{"768P": {}, "2K": {}}
	mioraAspectRatios = map[string]struct{}{
		"adaptive": {}, "21:9": {}, "16:9": {}, "4:3": {}, "1:1": {}, "3:4": {}, "9:16": {},
	}
)

// mioraBase 生效的 miora 代理基址：Client.MioraBase 非空取之，否则默认。
func (c *Client) mioraBase() string {
	if b := strings.TrimSpace(c.MioraBase); b != "" {
		return strings.TrimRight(b, "/")
	}
	return mioraBaseDefault
}

// IsMioraVendor 报告 vendor 是否指向 miora 通道（大小写不敏感；空厂商不算）。
func IsMioraVendor(vendor string) bool {
	return strings.EqualFold(strings.TrimSpace(vendor), MioraVideoVendor)
}

// MioraVideoVendorPrefix 返回目录 ID 的 miora 前缀（"miora-"）。
func MioraVideoVendorPrefix() string { return strings.ToLower(MioraVideoVendor) + "-" }

// VideoVendors 返回生视频支持的厂商清单（供 server 侧前缀解析，顺序即匹配优先级）。
func VideoVendors() []string { return []string{DefaultVideoVendor, MioraVideoVendor} }

// signClientTempKey 用账号 accessToken 换一枚 ck_t_ 临时密钥。
//
// 与 chat 同域（chatBase），但与 miora 媒体端点（mioraBase）分离：
// 签发域只在 /v2/api-keys 上，媒体域名只认这枚 Bearer。
func (c *Client) signClientTempKey(ctx context.Context, a *auth.Auth) (string, error) {
	if a == nil || strings.TrimSpace(a.AccessToken) == "" {
		return "", &Error{Kind: ErrAccountFault, Status: http.StatusUnauthorized, Msg: "account has no access token"}
	}
	body := map[string]any{
		"name":              mioraTempKeyName,
		"expire_in_seconds": mioraTempKeyTTLSeconds,
		"temporary":         true,
	}
	raw, err := c.postJSONCtx(ctx, a, c.chatBase(a)+mioraAPIKeysPath, body, mediaImageHeaders)
	if err != nil {
		return "", err
	}
	var parsed struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("parse temp key response: %w", err)
	}
	if !strings.HasPrefix(parsed.Key, mioraTempKeyPrefix) {
		return "", &Error{Kind: ErrAccountFault, Status: http.StatusBadGateway,
			Msg: "temp key response did not contain a " + mioraTempKeyPrefix + " key"}
	}
	return parsed.Key, nil
}

// mioraEnvelope miora 代理响应信封。
//
// 注意两点与常规上游不同：
//   - 鉴权失败时**没有** failed 字段，只有非零字符串 code（如 AUTH_MISSING_TEMP_KEY）；
//   - 业务失败时 HTTP 可能是 200，错误态在信封内（failed=true + httpStatus=502 等）。
//
// 故成功判定必须同时看 code==0 且 !failed。
type mioraEnvelope struct {
	Code       json.RawMessage `json:"code"`
	Failed     bool            `json:"failed"`
	Message    string          `json:"message"`
	Data       json.RawMessage `json:"data"`
	HTTPStatus int             `json:"httpStatus"`
}

// mioraCodeOK 报告信封 code 是否为数值 0（字符串形态的 code 一律视为失败）。
func mioraCodeOK(raw json.RawMessage) bool {
	var n int
	return json.Unmarshal(raw, &n) == nil && n == 0
}

// mioraErrorKind 把 miora 错误码/文案归一到网关错误分类，决定是否罚号/轮转。
func mioraErrorKind(code, msg string) ErrKind {
	upper := strings.ToUpper(code)
	switch {
	case strings.Contains(upper, "AUTH"):
		return ErrAccountFault
	case strings.Contains(upper, "CONTENT_SAFETY"), strings.Contains(upper, "COPYRIGHT"):
		return ErrContentBlocked
	}
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "requires prompt"),
		strings.Contains(low, "unsupported"),
		strings.Contains(low, "invalid"),
		strings.Contains(low, "missing"),
		strings.Contains(low, "too long"):
		if strings.Contains(low, "too long") {
			return ErrPromptTooLong
		}
		return ErrBadParams
	case strings.Contains(low, "safety"), strings.Contains(low, "copyright"),
		strings.Contains(low, "敏感"), strings.Contains(low, "违规"):
		return ErrContentBlocked
	}
	return ErrServer
}

// mioraPostCtx 发一次 miora 代理请求（Bearer ck_t_），解 {code,failed,data} 信封返回 data 原文。
func (c *Client) mioraPostCtx(ctx context.Context, ck, path string, body any) (json.RawMessage, error) {
	req, err := c.newJSONReq(ctx, c.mioraBase()+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+ck)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	var env mioraEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 160))
	}
	if env.Failed || !mioraCodeOK(env.Code) {
		status := env.HTTPStatus
		if status < 400 {
			status = resp.StatusCode
		}
		msg := strings.TrimSpace(env.Message)
		if msg == "" {
			msg = truncate(string(raw), 300)
		}
		kind := mioraErrorKind(string(env.Code), msg)
		// 参数类错误回 400（而不是信封里的 502），让客户端看到「请求问题」而非「上游故障」。
		if kind == ErrBadParams || kind == ErrPromptTooLong || kind == ErrContentBlocked {
			status = http.StatusBadRequest
		}
		return nil, &Error{Kind: kind, Status: status, Msg: msg}
	}
	return env.Data, nil
}

// SubmitVideoMiora 提交一次 miora H3 文生视频任务，返回上游 upstreamTaskId。
//
// model 为空回落 MioraVideoModel（上游按 prompt 自动选，此处仅作占位）。
// durationSec <= 0 时不带时长字段（上游默认）；带值时必须落在 4-15s。
// resolution / aspectRatio 为空则不带（上游默认），带值必须命中白名单。
func (c *Client) SubmitVideoMiora(ctx context.Context, a *auth.Auth, prompt, model string, durationSec int, resolution, aspectRatio string) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", &Error{Kind: ErrBadParams, Status: http.StatusBadRequest, Msg: "prompt is required"}
	}
	if strings.TrimSpace(model) == "" {
		model = MioraVideoModel
	}
	_ = model // 上游按 prompt 选模型，model 仅作目录标识，不入请求体。

	body := map[string]any{"prompt": strings.TrimSpace(prompt)}
	if durationSec > 0 {
		if durationSec < mioraDurationMin || durationSec > mioraDurationMax {
			return "", &Error{Kind: ErrBadParams, Status: http.StatusBadRequest,
				Msg: fmt.Sprintf("miora video duration must be %d-%d seconds, got %d", mioraDurationMin, mioraDurationMax, durationSec)}
		}
		body["duration"] = durationSec
	}
	if r := strings.TrimSpace(resolution); r != "" {
		norm := strings.ToUpper(r)
		if _, ok := mioraResolutions[norm]; !ok {
			return "", &Error{Kind: ErrBadParams, Status: http.StatusBadRequest,
				Msg: "unsupported resolution " + r + ", supported values: [768P 2K]"}
		}
		body["resolution"] = norm
	}
	if ar := strings.TrimSpace(aspectRatio); ar != "" {
		if _, ok := mioraAspectRatios[strings.ToLower(ar)]; !ok {
			return "", &Error{Kind: ErrBadParams, Status: http.StatusBadRequest,
				Msg: "unsupported aspect_ratio " + ar + ", supported values: [adaptive 21:9 16:9 4:3 1:1 3:4 9:16]"}
		}
		body["aspect_ratio"] = ar
	}

	ck, err := c.signClientTempKey(ctx, a)
	if err != nil {
		return "", err
	}
	raw, err := c.mioraPostCtx(ctx, ck, mioraVideoSubmitPath, body)
	if err != nil {
		return "", err
	}
	var res struct {
		UpstreamTaskID string `json:"upstreamTaskId"`
		Model          string `json:"model"`
		Provider       string `json:"provider"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("parse miora submit response: %w", err)
	}
	if res.UpstreamTaskID == "" {
		return "", fmt.Errorf("miora returned empty upstream task id")
	}
	return res.UpstreamTaskID, nil
}

// QueryVideoMiora 查询 miora 视频任务状态。
//
// 归一状态到网关三态（DONE / FAIL / RUNNING）。注意：pending 条目可能带一条
// 无害的 errorMessage（代理侧对未知 id 的回退文案），**不能**据此判失败——
// 只有终态 FAIL 才把 errorMessage 落进 ErrorMsg。
func (c *Client) QueryVideoMiora(ctx context.Context, a *auth.Auth, taskID string) (*VideoJob, error) {
	if strings.TrimSpace(taskID) == "" {
		return nil, &Error{Kind: ErrBadParams, Status: http.StatusBadRequest, Msg: "task id is required"}
	}
	ck, err := c.signClientTempKey(ctx, a)
	if err != nil {
		return nil, err
	}
	raw, err := c.mioraPostCtx(ctx, ck, mioraVideoQueryPath, map[string]any{"upstreamTaskIds": []string{taskID}})
	if err != nil {
		return nil, err
	}
	var data map[string]struct {
		Status       string `json:"status"`
		ErrorMessage string `json:"errorMessage"`
		ResultFiles  []struct {
			SignedURL string `json:"signedUrl"`
			MimeType  string `json:"mimeType"`
		} `json:"resultFiles"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("parse miora query response: %w", err)
	}
	entry, ok := data[taskID]
	if !ok {
		return nil, fmt.Errorf("miora query returned no entry for task %s", taskID)
	}
	job := &VideoJob{JobID: taskID, Status: mioraVideoStatus(entry.Status)}
	if len(entry.ResultFiles) > 0 {
		job.ResultURL = entry.ResultFiles[0].SignedURL
	}
	switch job.Status {
	case "FAIL":
		job.ErrorMsg = entry.ErrorMessage
	case "DONE":
		if job.ResultURL == "" {
			job.Status = "FAIL"
			job.ErrorMsg = "miora reported completed but returned no result file"
		}
	}
	return job, nil
}

// mioraVideoStatus 把 miora 业务状态归一到网关三态（大写，供 VideoJob.Done/Failed 判定）。
func mioraVideoStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "completed", "succeeded", "success", "done":
		return "DONE"
	case "failed", "error", "canceled", "cancelled":
		return "FAIL"
	default: // pending / processing / queued / running / 未知
		return "RUNNING"
	}
}
