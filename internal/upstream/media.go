// media.go 上游多模态（生图 / 生视频）通道。
//
// 三条链路的鉴权形态完全不同，不要混用：
//   - 生图 /v2/images/generations：与 chat 同族的 Bearer accessToken，同步返回图片 URL（仅文生图）。
//   - miora 生图/改图（media_miora_image.go）：自签 ck_t_ 临时密钥 + workbuddy-proxy，
//     文生图与改图两条端点，`mode=sync` 时产物直接在 submit 响应里返回。
//   - 生视频 /agenttool/v1/tcproxy：TC3-HMAC-SHA256 签名代理（腾讯云转发），
//     secret_id = "<provider>.<accessToken>"，secret_key 内置固定串；
//     提交返回 JobId，需再轮询 DescribeAigcVideoJob 取产物。
//
// 常量取值来自官方客户端脚本
// workbuddy-builtin/skills/buddy-multimodal-generation/scripts/buddy-cloud.py
// （本地实测两条链路均 200，见 .workbuddy/memory/2026-10-13.md）。
package upstream

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	// imagesGenerationsPath 上游生图端点（Bearer 鉴权，OpenAI 近似形态）。
	imagesGenerationsPath = "/v2/images/generations"
	// tcproxyPath 腾讯云 API 转发代理端点（TC3 签名鉴权）。
	tcproxyPath = "/agenttool/v1/tcproxy"

	// tcRegion tcproxy 固定区域（脚本内置，不可改）。
	tcRegion = "ap-guangzhou"
	// tcSigningKey TC3 secret_key（官方客户端硬编码，signing key 前缀 TC3 拼接）。
	tcSigningKey = "codebuddy"

	// DefaultImageModel 生图默认模型 ID。上游不认空 model（会报 14401）。
	DefaultImageModel = "hunyuan-image-alpha"
	// DefaultVideoVendor / DefaultVideoModel 生视频默认厂商与版本（官方脚本默认）。
	DefaultVideoVendor = "Kling"
	DefaultVideoModel  = "v2.6"
)

// 多模态 provider/service/version/action 映射（官方脚本 _PROVIDER_MAP 逐字）。
const (
	imageProvider = "hy-aiart"
	imageService  = "aiart"

	videoProvider      = "video-effect"
	videoService       = "vclm"
	videoAPIVersion    = "2024-05-23"
	videoSubmitAction  = "SubmitAigcVideoJob"
	videoQueryAction   = "DescribeAigcVideoJob"
	tcActionHeaderName = "X-TC-Action"
)

// MediaModel 多模态（生图/生视频）模型目录项。
//
// 与 chat 模型不同：这些模型由 /v2/images/generations 与 tcproxy 驱动，
// 不在上游 chat 模型列表里，故由网关本地静态登记。HTTP 层（/v1/models）
// 与面板共用本清单，避免两侧各写一份而漂移。
type MediaModel struct {
	ID          string // 裸模型/目录 ID（HTTP 层按 realm 加前缀）
	Type        string // "image" / "video"
	Capability  string // 机器可读能力标记
	Description string
}

// MediaModelCatalog 返回网关支持的多模态模型清单。
func MediaModelCatalog() []MediaModel {
	return []MediaModel{
		{
			ID:          DefaultImageModel,
			Type:        "image",
			Capability:  "image_generation",
			Description: "腾讯混元生图（同步 POST /v1/images/generations）",
		},
		{
			ID:          VideoVendorPrefix() + DefaultVideoModel,
			Type:        "video",
			Capability:  "video_generation",
			Description: "可灵 " + DefaultVideoModel + " 文生视频（异步 POST /v1/videos/generations + GET /v1/videos/generations/{id}）",
		},
		{
			ID:          MioraVideoVendorPrefix() + MioraVideoModel,
			Type:        "video",
			Capability:  "video_generation",
			Description: "MiniMax H3 视频（miora 代理，支持文生/首尾帧/参考生；异步 POST /v1/videos/generations + GET /v1/videos/generations/{id}）",
		},
		{
			ID:          MioraImageModel,
			Type:        "image",
			Capability:  "image_generation",
			Description: "Miora 生图（同步 POST /v1/images/generations；也可以直接看 OpenAI 风格的 POST /v1/images/edits）",
		},
		{
			ID:          MioraImageEditModel,
			Type:        "image",
			Capability:  "image_edit",
			Description: "Miora 改图/重绘（给图 + 指令改图；OpenAI 风格 POST /v1/images/edits，multipart 或 JSON）",
		},
	}
}

// VideoVendorPrefix 返回生视频目录 ID 的厂商前缀（小写 vendor + "-"）。
// 与 server 侧 parseVideoModel 的前缀解析互为逆运算，集中在此避免两处硬编码。
func VideoVendorPrefix() string {
	return strings.ToLower(DefaultVideoVendor) + "-"
}

// MediaUsage 一次媒体生成的上游计量（字段名与上游 usage 一致）。
type MediaUsage struct {
	InputTokens  int64   `json:"input_tokens,omitempty"`
	OutputTokens int64   `json:"output_tokens,omitempty"`
	TotalTokens  int64   `json:"total_tokens,omitempty"`
	Credit       float64 `json:"credit,omitempty"`
}

// ImageResult 单张生成图片。
type ImageResult struct {
	URL           string
	RevisedPrompt string
}

// GenerateImage 调上游同步生图端点。
//
// model 为空时回落 DefaultImageModel；n <= 0 时不发送张数字段（上游默认 1 张）。
// 返回的 URL 是对象存储临时签名地址（约 7 天有效），调用方需及时转存。
func (c *Client) GenerateImage(ctx context.Context, a *auth.Auth, prompt, model, size string, n int) ([]ImageResult, MediaUsage, error) {
	if strings.TrimSpace(prompt) == "" {
		return nil, MediaUsage{}, &Error{Kind: ErrBadParams, Status: http.StatusBadRequest, Msg: "prompt is required"}
	}
	if strings.TrimSpace(model) == "" {
		model = DefaultImageModel
	}
	body := map[string]any{"model": model, "prompt": prompt}
	if size != "" {
		body["size"] = size
	}
	if n > 0 {
		body["n"] = n
	}

	raw, err := c.postJSONCtx(ctx, a, c.chatBase(a)+imagesGenerationsPath, body, mediaImageHeaders)
	if err != nil {
		return nil, MediaUsage{}, err
	}
	// data: {"created":..,"data":[{"url":..,"revised_prompt":..}],"usage":{..}}
	var parsed struct {
		Data []struct {
			URL           string `json:"url"`
			RevisedPrompt string `json:"revised_prompt"`
		} `json:"data"`
		Usage MediaUsage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, MediaUsage{}, fmt.Errorf("parse image response: %w", err)
	}
	out := make([]ImageResult, 0, len(parsed.Data))
	for _, d := range parsed.Data {
		if d.URL != "" {
			out = append(out, ImageResult{URL: d.URL, RevisedPrompt: d.RevisedPrompt})
		}
	}
	if len(out) == 0 {
		return nil, parsed.Usage, fmt.Errorf("upstream returned no image url")
	}
	return out, parsed.Usage, nil
}

// VideoJob 视频任务状态快照（字段语义对齐上游 Response）。
type VideoJob struct {
	JobID     string
	Status    string // 上游原样：如 RUNNING / DONE / FAIL
	ResultURL string // Status=DONE 时为产物 mp4 地址（对象存储临时签名）
	ErrorCode string
	ErrorMsg  string
}

// Done 报告任务是否已产出可用结果。
func (j *VideoJob) Done() bool {
	return j != nil && j.ResultURL != "" && strings.EqualFold(j.Status, "DONE")
}

// Failed 报告任务是否明确失败。
func (j *VideoJob) Failed() bool {
	if j == nil {
		return false
	}
	if j.ErrorCode != "" || j.ErrorMsg != "" {
		return true
	}
	switch strings.ToUpper(j.Status) {
	case "FAIL", "FAILED", "ERROR":
		return true
	}
	return false
}

// SubmitVideo 提交文生视频任务，返回上游 JobId。
// vendor/model 为空时回落默认；durationSec <= 0 时不带时长参数（上游默认 5 秒）。
func (c *Client) SubmitVideo(ctx context.Context, a *auth.Auth, prompt, vendor, model string, durationSec int) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", &Error{Kind: ErrBadParams, Status: http.StatusBadRequest, Msg: "prompt is required"}
	}
	if vendor == "" {
		vendor = DefaultVideoVendor
	}
	if model == "" {
		model = DefaultVideoModel
	}
	body := map[string]any{
		"Vendor":  vendor,
		"Model":   model,
		"Prompt":  prompt,
		"LogoAdd": 1,
	}
	if durationSec > 0 {
		param, _ := json.Marshal(map[string]any{"Duration": durationSec})
		body["ModelParam"] = string(param)
	}
	var resp struct {
		Response struct {
			JobID     string `json:"JobId"`
			RequestID string `json:"RequestId"`
		} `json:"Response"`
	}
	if err := c.tcproxyCtx(ctx, a, videoProvider, videoService, videoSubmitAction, videoAPIVersion, body, &resp); err != nil {
		return "", err
	}
	if resp.Response.JobID == "" {
		return "", fmt.Errorf("upstream returned empty video job id")
	}
	return resp.Response.JobID, nil
}

// QueryVideo 查询视频任务状态（上游 DescribeAigcVideoJob）。
func (c *Client) QueryVideo(ctx context.Context, a *auth.Auth, jobID string) (*VideoJob, error) {
	if strings.TrimSpace(jobID) == "" {
		return nil, &Error{Kind: ErrBadParams, Status: http.StatusBadRequest, Msg: "job id is required"}
	}
	var resp struct {
		Response struct {
			Status    string `json:"Status"`
			ResultURL string `json:"ResultUrl"`
			ErrorCode string `json:"ErrorCode"`
			ErrorMsg  string `json:"ErrorMessage"`
			RequestID string `json:"RequestId"`
		} `json:"Response"`
	}
	if err := c.tcproxyCtx(ctx, a, videoProvider, videoService, videoQueryAction, videoAPIVersion, map[string]any{"JobId": jobID}, &resp); err != nil {
		return nil, err
	}
	return &VideoJob{
		JobID:     jobID,
		Status:    resp.Response.Status,
		ResultURL: resp.Response.ResultURL,
		ErrorCode: resp.Response.ErrorCode,
		ErrorMsg:  resp.Response.ErrorMsg,
	}, nil
}

// ---------------------------------------------------------------------------
// 请求发送
// ---------------------------------------------------------------------------

// mediaHeaders 标注媒体请求的头族，供 postJSONCtx 选择注入策略。
type mediaHeaders int

const (
	// mediaImageHeaders /v2/images/generations：与 chat 同域的 Bearer 头族。
	mediaImageHeaders mediaHeaders = iota
	// mediaTCHeaders tcproxy：只发 TC3 签名头（官方脚本逐字，已验证）。
	mediaTCHeaders
)

// postJSONCtx 发送 JSON POST 并解 {code,msg,data} 信封，返回 data 原文。
// ctx 生效（客户端断连即取消，不空跑上游）。
func (c *Client) postJSONCtx(ctx context.Context, a *auth.Auth, endpoint string, body any, hdr mediaHeaders) (json.RawMessage, error) {
	req, err := c.newJSONReq(ctx, endpoint, body)
	if err != nil {
		return nil, err
	}
	switch hdr {
	case mediaImageHeaders:
		c.mediaImageHeaders(req, a)
	case mediaTCHeaders:
		// 由 tcproxyCtx 自行签名，此处不应到达。
	}
	return c.doJSONCtx(req)
}

// newJSONReq 构造带 JSON body 的 POST 请求。
func (c *Client) newJSONReq(ctx context.Context, endpoint string, body any) (*http.Request, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	return req, nil
}

// mediaImageHeaders 注入生图端点的 Bearer 头族。
// 基线取 CommonHeaders（Origin/Referer/UA/X-CodeBuddy-Request/设备头），
// 再补 chat 同族的 Authorization / X-User-Id / X-Domain。
func (c *Client) mediaImageHeaders(req *http.Request, a *auth.Auth) {
	c.CommonHeaders(req, a)
	req.Header.Set("Accept", "application/json")
	if a != nil && a.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	}
	if a != nil && a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a != nil && a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	}
}

// doJSONCtx 同 doJSON，但走请求自带 ctx。
func (c *Client) doJSONCtx(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 300)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 160))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode,
			Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 200))}
	}
	return env.Data, nil
}

// tcproxyCtx 走 TC3 签名代理提交一次腾讯云 API 调用。
// out 接收 {"Response":{...}} 形态的响应体。
func (c *Client) tcproxyCtx(ctx context.Context, a *auth.Auth, provider, service, action, version string, body any, out any) error {
	endpoint := c.chatBase(a) + tcproxyPath
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	host := tcproxyHost(endpoint)
	for k, v := range signTC3(provider, accessTokenOf(a), service, action, version, host, string(payload), time.Now()) {
		// Host 由 http.Client 按 URL 自动填（值同签名所用 host），不能经 Header 覆盖。
		if k == "Host" {
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return &Error{Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode,
			Msg: truncate(string(raw), 300)}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("parse tcproxy response: %w (body: %s)", err, truncate(string(raw), 160))
	}
	return nil
}

// tcproxyHost 取 URL 主机名（TC3 签名 host 段）。
func tcproxyHost(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		return u.Host
	}
	return "copilot.tencent.com"
}

// accessTokenOf 取账号 token（空账号返回空串，签名必然失败但不会 panic）。
func accessTokenOf(a *auth.Auth) string {
	if a == nil {
		return ""
	}
	return a.AccessToken
}

// ---------------------------------------------------------------------------
// TC3-HMAC-SHA256 签名
// ---------------------------------------------------------------------------

// signTC3 生成 TC3-HMAC-SHA256 授权头族。
//
// 与官方脚本逐字对齐的三处细节：
//   - SignedHeaders 固定 "content-type;host;x-tc-action"（只签这三项）；
//   - secret_id = "<provider>.<accessToken>"（provider 前缀是代理侧路由键）；
//   - signing key 首段为 "TC3" + secret_key 拼接。
func signTC3(provider, token, service, action, version, host, payload string, now time.Time) map[string]string {
	ts := now.Unix()
	date := now.UTC().Format("2006-01-02")

	const contentType = "application/json; charset=utf-8"
	const signedHeaders = "content-type;host;x-tc-action"

	canonicalHeaders := "content-type:" + contentType + "\n" +
		"host:" + host + "\n" +
		"x-tc-action:" + strings.ToLower(action) + "\n"
	canonicalRequest := "POST\n/\n\n" + canonicalHeaders + "\n" + signedHeaders + "\n" +
		sha256Hex([]byte(payload))

	scope := date + "/" + service + "/tc3_request"
	stringToSign := "TC3-HMAC-SHA256\n" + strconv.FormatInt(ts, 10) + "\n" + scope + "\n" +
		sha256Hex([]byte(canonicalRequest))

	secretDate := hmacSHA256([]byte("TC3"+tcSigningKey), []byte(date))
	secretService := hmacSHA256(secretDate, []byte(service))
	secretSigning := hmacSHA256(secretService, []byte("tc3_request"))
	signature := hex.EncodeToString(hmacSHA256(secretSigning, []byte(stringToSign)))

	authorization := "TC3-HMAC-SHA256 Credential=" + provider + "." + token + "/" + scope +
		", SignedHeaders=" + signedHeaders + ", Signature=" + signature

	return map[string]string{
		"Authorization":     authorization,
		"Content-Type":      contentType,
		"Host":              host,
		tcActionHeaderName:  action,
		"X-TC-Version":      version,
		"X-TC-Region":       tcRegion,
		"X-TC-Timestamp":    strconv.FormatInt(ts, 10),
	}
}

// sha256Hex 返回 data 的 sha256 十六进制摘要。
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// hmacSHA256 返回 HMAC-SHA256 原始摘要。
func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}
