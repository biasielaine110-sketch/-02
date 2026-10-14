// media_miora_image.go miora 生图 / 改图通道——与 miora 视频并列的第三条媒体后端。
//
// 机制与 miora 视频**完全同源**（同一 `ck_t_` 临时密钥、同一 /api/ai/workbuddy-proxy
// 代理域、同一 {code,failed,message,data} 信封、同一 MediaPayload 形状），只是端点不同：
//
//	文生图  POST /api/ai/workbuddy-proxy/image/generate/submit  {prompt[,size][,quality]}
//	改图    POST /api/ai/workbuddy-proxy/image/edit/submit      {prompt, images:[MediaPayload…][,size]}
//	查询    POST /api/ai/workbuddy-proxy/image/query-task       {upstreamTaskIds:[id]}（与视频共用）
//
// ⚠ 与视频最大的差异：**图片走 `mode=sync`，产物在 submit 响应里直接返回**
// （data.status=completed + data.resultFiles[0].signedUrl）。把 sync 任务的 id 拿去
// query 会得到 `INVALID_TASK_ID_FOR_SYNC_MODE` —— 那是协议如此，不是故障。
// 故本层默认一次请求定结果；仅当上游明明回了非 sync 且未终态时才落回轮询。
//
// 契约来源：安装目录里的官方 miora MCP 实现（源码路径注释齐全）
// `<WorkBuddy>/resources/app.asar.unpacked/resources/plugins/workbuddy-builtin/mcps/
//  miora-mcp/dist/cli.cjs` 的 `src/media/miora-paths.ts` / `tools/text-to-image-tool.ts` /
// `tools/edit-image-tool.ts`；本地真实账号实测两条链路均 code=0 且返回真实 COS signedUrl
// （见 .workbuddy/memory/2026-10-14.md）。
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	// mioraImageGeneratePath / mioraImageEditPath 生图与改图的提交端点。
	mioraImageGeneratePath = mioraProxyBase + "/image/generate/submit"
	mioraImageEditPath     = mioraProxyBase + "/image/edit/submit"
	// mioraImageQueryPath 图片任务查询端点（与视频**共用**同一条 query-task）。
	mioraImageQueryPath = mioraProxyBase + "/image/query-task"

	// MioraImageModel / MioraImageEditModel miora 图片目录 ID。
	// 上游实际回报 model=hy-image-3.5-wb（文生）/ hy-image-3.5-edit-wb（改图）。
	MioraImageModel     = "miora-image"
	MioraImageEditModel = "miora-image-edit"

	// mioraImagePollInterval 异步兜底时的轮询间隔（官方 MCP 用 2500ms）。
	mioraImagePollInterval = 2500 * time.Millisecond
)

// IsMioraImageModel 报告模型规格是否指向 miora 图片通道（目录 ID 前缀 "miora-image"）。
//
// 注意与视频目录 ID `miora-minimax-h3` 区分：这里只认 "miora-image" 前缀，
// 故视频模型名不会误入图片通道。
func IsMioraImageModel(spec string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(spec)), strings.ToLower(MioraImageModel))
}

// MioraImageVendorPrefix 返回图片目录 ID 的厂商前缀（"miora-"），与视频同前缀。
func MioraImageVendorPrefix() string { return strings.ToLower(MioraVideoVendor) + "-" }

// MioraImageRequest 一次 miora 图片提交的全部入参。
//
// Images 用**指针**区分「文生图（nil）」与「改图（非 nil，须非空）」：
// 上游对空的 images 会回 MISSING_INPUT（实测：`需要至少一张参考图（images[]）`），
// 不做静默降级；但本地仍前置拦一道，避免把明显的调用错误送到上游浪费一次往返。
type MioraImageRequest struct {
	Prompt  string
	Size    string // "WIDTHxHEIGHT"，如 1024x1024；空 = 上游默认
	Quality string // 上游质量档（如 standard / hd）；空 = 上游默认
	Images  *[]MediaPayload
}

// MioraImageResult 一次 miora 图片生成的结果快照。
type MioraImageResult struct {
	TaskID    string
	Model     string
	Provider  string
	Status    string // 归一三态：DONE / FAIL / RUNNING
	URL       string // resultFiles[0].signedUrl（COS 临时直链，expiresAt 约 1 小时后失效）
	MimeType  string
	ExpiresAt int64
	ErrorMsg  string
}

// mioraImageEntry 上游图片 data 段（submit 与 query 的条目同形）。
type mioraImageEntry struct {
	Status         string `json:"status"`
	Mode           string `json:"mode"`
	Model          string `json:"model"`
	Provider       string `json:"provider"`
	ErrorMessage   string `json:"errorMessage"`
	UpstreamTaskID string `json:"upstreamTaskId"`
	ResultFiles    []struct {
		SignedURL string `json:"signedUrl"`
		MimeType  string `json:"mimeType"`
		ExpiresAt int64  `json:"expiresAt"`
	} `json:"resultFiles"`
}

// SubmitImageMiora 提交一次 miora 图片任务（文生图 / 改图），按需返回终态结果。
//
// Images 为 nil → 文生图（/image/generate/submit）；非 nil → 改图（/image/edit/submit），
// 数组须非空且每项载荷恰含 base64/url/file_key 之一（否则本地 400，不发上游）。
//
// 返回态：上游 `mode=sync` 时结果已在响应内，直接返回 DONE/FAIL；
// 仅当上游回了非 sync 且状态未终态时才轮询 /image/query-task（受 ctx 约束）。
func (c *Client) SubmitImageMiora(ctx context.Context, a *auth.Auth, req MioraImageRequest) (*MioraImageResult, error) {
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return nil, &Error{Kind: ErrBadParams, Status: http.StatusBadRequest, Msg: "prompt is required"}
	}

	body := map[string]any{"prompt": prompt}
	path := mioraImageGeneratePath
	if req.Images != nil {
		imgs := *req.Images
		if len(imgs) == 0 {
			return nil, &Error{Kind: ErrBadParams, Status: http.StatusBadRequest,
				Msg: "miora: image edit requires at least one image"}
		}
		normalized := make([]MediaPayload, 0, len(imgs))
		for _, im := range imgs {
			n, err := im.normalized()
			if err != nil {
				return nil, err
			}
			normalized = append(normalized, n)
		}
		body["images"] = normalized
		path = mioraImageEditPath
	}
	if s := strings.TrimSpace(req.Size); s != "" {
		body["size"] = s
	}
	if q := strings.TrimSpace(req.Quality); q != "" {
		body["quality"] = q
	}

	ck, err := c.signClientTempKey(ctx, a)
	if err != nil {
		return nil, err
	}
	raw, err := c.mioraPostCtx(ctx, ck, path, body)
	if err != nil {
		return nil, err
	}
	var entry mioraImageEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return nil, fmt.Errorf("parse miora image response: %w", err)
	}
	return c.finishImageMiora(ctx, ck, entry)
}

// finishImageMiora 判定一次图片提交是否已定结果；同步直接出，异步兜底轮询。
func (c *Client) finishImageMiora(ctx context.Context, ck string, entry mioraImageEntry) (*MioraImageResult, error) {
	// sync：产物就在 submit 响应里。不能拿它去 query（上游会回 INVALID_TASK_ID_FOR_SYNC_MODE）。
	if strings.EqualFold(strings.TrimSpace(entry.Mode), "sync") || mioraImageTerminal(entry.Status) {
		return mioraImageResultFromEntry(entry)
	}
	if strings.TrimSpace(entry.UpstreamTaskID) == "" {
		return nil, fmt.Errorf("miora image submit returned neither a sync result nor an upstreamTaskId")
	}
	polled, err := c.pollImageMiora(ctx, ck, entry.UpstreamTaskID)
	if err != nil {
		return nil, err
	}
	return mioraImageResultFromEntry(polled)
}

// pollImageMiora 轮询图片任务直到终态（或 ctx 结束）。
func (c *Client) pollImageMiora(ctx context.Context, ck, taskID string) (mioraImageEntry, error) {
	var last mioraImageEntry
	for {
		raw, err := c.mioraPostCtx(ctx, ck, mioraImageQueryPath,
			map[string]any{"upstreamTaskIds": []string{taskID}})
		if err != nil {
			return last, err
		}
		var data map[string]mioraImageEntry
		if err := json.Unmarshal(raw, &data); err != nil {
			return last, fmt.Errorf("parse miora image query response: %w", err)
		}
		entry, ok := data[taskID]
		if !ok {
			return last, fmt.Errorf("miora image query returned no entry for task %s", taskID)
		}
		last = entry
		if mioraImageTerminal(entry.Status) {
			return entry, nil
		}
		select {
		case <-ctx.Done():
			return last, &Error{Kind: ErrServer, Status: http.StatusGatewayTimeout,
				Msg: "timed out waiting for miora image task " + taskID + " (last status=" + entry.Status + ")"}
		case <-time.After(mioraImagePollInterval):
		}
	}
}

// mioraImageTerminal 报告状态是否已定（completed / failed / cancelled）。
func mioraImageTerminal(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "succeeded", "success", "done", "failed", "error", "cancelled", "canceled":
		return true
	}
	return false
}

// mioraImageResultFromEntry 把上游条目翻成网关结果；completed 但无产物视为失败。
func mioraImageResultFromEntry(entry mioraImageEntry) (*MioraImageResult, error) {
	res := &MioraImageResult{
		TaskID:   entry.UpstreamTaskID,
		Model:    entry.Model,
		Provider: entry.Provider,
		Status:   mioraVideoStatus(entry.Status),
	}
	if len(entry.ResultFiles) > 0 {
		res.URL = strings.TrimSpace(entry.ResultFiles[0].SignedURL)
		res.MimeType = entry.ResultFiles[0].MimeType
		res.ExpiresAt = entry.ResultFiles[0].ExpiresAt
	}
	switch res.Status {
	case "FAIL":
		res.ErrorMsg = strings.TrimSpace(entry.ErrorMessage)
		if res.ErrorMsg == "" {
			res.ErrorMsg = "miora image task failed without a message"
		}
		return res, &Error{Kind: mioraErrorKind("", res.ErrorMsg), Status: http.StatusBadGateway, Msg: res.ErrorMsg}
	case "DONE":
		if res.URL == "" {
			res.Status = "FAIL"
			res.ErrorMsg = "miora reported completed but returned no result file"
			return res, &Error{Kind: ErrServer, Status: http.StatusBadGateway, Msg: res.ErrorMsg}
		}
	}
	return res, nil
}
