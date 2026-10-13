// media.go 媒体生成 HTTP 路由：生图（同步）与生视频（异步任务）。
//
// 与 chat 路径共用账号池与 Bearer 鉴权，但**不共享**错误分类的惩罚策略：
// 生图/生视频是低频高单价调用，上游返回的软限流（429）不与 chat 的
// 600s 级软冷却捆绑，改用统一的短冷却，避免一次生图限流把账号从 chat
// 池里长时间摘掉（chat 轮转读的是同一个池）。
package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// mediaMaxBodyBytes 生图请求体上限（prompt 文本很小，1MB 足够）。
const mediaMaxBodyBytes = 1 << 20

// videoMaxBodyBytes 生视频请求体上限。首尾帧/参考生可内联 base64 图片（data URI 或裸串），
// 一张 2K 图编码后约数 MB，24MB 可容纳数张；生图仍用更严的 1MB。
const videoMaxBodyBytes = 24 << 20

// mediaTimeout 单次媒体上游调用的总时长上限。
// 生图实测数秒；视频提交/查询都是短请求（产物由上游异步产出），30s 足够。
const mediaTimeout = 30 * time.Second

// imageDownloadTimeout b64_json 模式下回源下载图片的时长上限。
const imageDownloadTimeout = 60 * time.Second

// ---------------------------------------------------------------------------
// 生图 POST /v1/images/generations
// ---------------------------------------------------------------------------

// imageRequest OpenAI 兼容生图入参。
type imageRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n"`
	Size           string `json:"size"`
	ResponseFormat string `json:"response_format"` // "url"（默认）/ "b64_json"
}

func (h *Handler) imagesGenerations(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readMediaBody(w, r, "image", mediaMaxBodyBytes)
	if !ok {
		return
	}
	var req imageRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "parse request: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "prompt is required")
		return
	}
	realm, bare := resolveModel(req.Model)
	// 未指定模型时按 realm 回落默认生图模型。
	if bare == "" {
		bare = upstream.DefaultImageModel
	}

	imgs, uerr := h.runMedia(w, r, realm, func(ctx context.Context, a *auth.Auth) (any, error) {
		out, u, err := h.cfg.Upstream.GenerateImage(ctx, a, req.Prompt, bare, req.Size, req.N)
		if err != nil {
			return nil, err
		}
		return mediaImagePayload{Images: out, Usage: u}, nil
	})
	if uerr != nil {
		h.writeMediaError(w, uerr)
		return
	}
	payload := imgs.(mediaImagePayload)

	format := strings.TrimSpace(req.ResponseFormat)
	data := make([]map[string]any, 0, len(payload.Images))
	for _, im := range payload.Images {
		item := map[string]any{}
		if format == "b64_json" {
			b64, err := fetchAsBase64(r.Context(), im.URL)
			if err != nil {
				writeOpenAIError(w, http.StatusBadGateway, "image_fetch_failed",
					"image generated but download failed: "+err.Error())
				return
			}
			item["b64_json"] = b64
		} else {
			item["url"] = im.URL
		}
		if im.RevisedPrompt != "" {
			item["revised_prompt"] = im.RevisedPrompt
		}
		data = append(data, item)
	}

	resp := map[string]any{
		"created": time.Now().Unix(),
		"data":    data,
	}
	if payload.Usage.TotalTokens > 0 || payload.Usage.Credit > 0 {
		resp["usage"] = payload.Usage
	}
	writeJSON(w, http.StatusOK, resp)
}

// mediaImagePayload runMedia 的生图返回值载体。
type mediaImagePayload struct {
	Images []upstream.ImageResult
	Usage  upstream.MediaUsage
}

// ---------------------------------------------------------------------------
// 生视频 POST /v1/videos/generations（提交） + GET /v1/videos/generations/{id}（查询）
// ---------------------------------------------------------------------------

// videoRequest OpenAI 风格生视频入参（vendor/model/duration/resolution/aspect_ratio 为上游扩展字段）。
//
// miora H3 扩展（仅 cn:miora-* 生效，见 videosGenerations 的通道校验）：
//   - first_frame / last_frame：首尾帧图生视频（frame_to_video），首帧必填；
//   - references              ：多参考图生视频（reference_to_video），数组须非空。
// 三者取值可为字符串（http(s) URL / data URI / 裸 base64）或对象
// {"base64":…|"url":…|"file_key":…}，与上游 MediaPayload 同形（见 upstream.MediaPayload）。
// References 用指针以区分「未提供（文生）」与「提供了空数组（应报错）」。
type videoRequest struct {
	Model       string `json:"model"`
	Prompt      string `json:"prompt"`
	Vendor      string `json:"vendor"`
	Duration    int    `json:"duration"` // 秒
	Resolution  string `json:"resolution"`
	AspectRatio string `json:"aspect_ratio"`

	FirstFrame *upstream.MediaPayload   `json:"first_frame"`
	LastFrame  *upstream.MediaPayload   `json:"last_frame"`
	References *[]upstream.MediaPayload `json:"references"`
}

// hasMioraVisualInput 报告请求是否携带 miora 专属的帧/参考图入参。
func (req videoRequest) hasMioraVisualInput() bool {
	return req.FirstFrame != nil || req.LastFrame != nil || req.References != nil
}

func (h *Handler) videosGenerations(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readMediaBody(w, r, "video", videoMaxBodyBytes)
	if !ok {
		return
	}
	var req videoRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "parse request: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "prompt is required")
		return
	}
	realm, bare := resolveModel(req.Model)
	// 把目录 ID（如 "kling-v2.6" / "miora-minimax-h3"）解析成 vendor + model；显式 vendor 字段优先。
	vendor, model := parseVideoModel(bare)
	if strings.TrimSpace(req.Vendor) != "" {
		vendor = strings.TrimSpace(req.Vendor)
	}
	channel := videoJobVendor(vendor)

	// 首尾帧/参考生是 miora H3 专属模式；Kling（tcproxy）不接受这些字段，
	// 静默忽略会让调用方误以为图生视频生效，故本地直接拒绝。
	if req.hasMioraVisualInput() && channel != videoChannelMiora {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"first_frame/last_frame/references are only supported by cn:miora-* models")
		return
	}

	type submitResult struct {
		JobID   string
		UID     string
		Channel string
	}
	res, uerr := h.runMedia(w, r, realm, func(ctx context.Context, a *auth.Auth) (any, error) {
		var jobID string
		var err error
		if channel == videoChannelMiora {
			jobID, err = h.cfg.Upstream.SubmitVideoMiora(ctx, a, upstream.MioraVideoRequest{
				Prompt:          req.Prompt,
				Model:           model,
				DurationSec:     req.Duration,
				Resolution:      req.Resolution,
				AspectRatio:     req.AspectRatio,
				FirstFrame:      req.FirstFrame,
				LastFrame:       req.LastFrame,
				ReferenceImages: req.References,
			})
		} else {
			jobID, err = h.cfg.Upstream.SubmitVideo(ctx, a, req.Prompt, vendor, model, req.Duration)
		}
		if err != nil {
			return nil, err
		}
		return submitResult{JobID: jobID, UID: a.UID, Channel: channel}, nil
	})
	if uerr != nil {
		h.writeMediaError(w, uerr)
		return
	}
	sr := res.(submitResult)
	videoJobs.put(sr.JobID, sr.UID, sr.Channel)

	writeJSON(w, http.StatusOK, map[string]any{
		"id":         sr.JobID,
		"object":     "video",
		"created":    time.Now().Unix(),
		"status":     "submitted",
		"status_url": "/v1/videos/generations/" + sr.JobID,
	})
}

func (h *Handler) videosGenerationGet(w http.ResponseWriter, r *http.Request) {
	jobID := strings.TrimSpace(r.PathValue("id"))
	if jobID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "job id is required")
		return
	}
	uid, channel, ok := videoJobs.get(jobID)
	if !ok {
		// 进程重启后任务表清空（进程内状态，不落盘）。
		// 明确回报「未知任务」而不是编造状态：调用方需重新提交。
		writeOpenAIError(w, http.StatusNotFound, "job_not_found",
			"unknown video job (the gateway may have restarted); please submit a new job")
		return
	}
	acct := h.cfg.Pool.PickByUID(uid)
	if acct == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account",
			"the account that owns this job is temporarily unavailable")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), mediaTimeout)
	defer cancel()
	var job *upstream.VideoJob
	var err error
	if channel == videoChannelMiora {
		job, err = h.cfg.Upstream.QueryVideoMiora(ctx, acct, jobID)
	} else {
		job, err = h.cfg.Upstream.QueryVideo(ctx, acct, jobID)
	}
	if err != nil {
		h.writeMediaError(w, err)
		return
	}

	out := map[string]any{
		"id":              job.JobID,
		"object":          "video",
		"status":          mediaVideoStatus(job),
		"upstream_status": job.Status,
	}
	if job.ResultURL != "" {
		out["url"] = job.ResultURL
	}
	if job.Failed() {
		out["error"] = map[string]any{
			"code":    job.ErrorCode,
			"message": job.ErrorMsg,
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// mediaVideoStatus 把上游状态归一到 OpenAI 风格三态，未知原样透出。
func mediaVideoStatus(job *upstream.VideoJob) string {
	switch {
	case job.Done():
		return "succeeded"
	case job.Failed():
		return "failed"
	}
	return "running"
}

// ---------------------------------------------------------------------------
// 任务表（jobID -> 提交账号 uid）
// ---------------------------------------------------------------------------

// videoJobRegistry 记录任务归属账号与通道，供 GET 查询时选回同一个号、走对后端。
// 为什么必须记账号：上游查询（DescribeAigcVideoJob / miora query-task）需要
// **提交该任务的账号**鉴权，换号查会取不到结果。
// 为什么必须记通道：Kling 走 tcproxy、miora 走 workbuddy-proxy，两者的任务 ID
// 互不通用，查错后端必然失败。
// 进程内状态、重启即清（重启后查询回 job_not_found）。
type videoJobRegistry struct {
	mu    sync.Mutex
	items map[string]videoJobEntry
}

type videoJobEntry struct {
	uid     string
	channel string
	at      time.Time
}

// 视频通道标识（存入任务表，查询时据此选后端）。
const (
	videoChannelKling = "kling"
	videoChannelMiora = "miora"
)

// videoJobVendor 归一任务所属通道：miora → videoChannelMiora，其余（Kling/空）→ videoChannelKling。
func videoJobVendor(vendor string) string {
	if upstream.IsMioraVendor(vendor) {
		return videoChannelMiora
	}
	return videoChannelKling
}

// videoJobTTL 任务表保留时长（上游产物 URL 约 7 天有效，这里留足重试窗口）。
const videoJobTTL = 24 * time.Hour

// videoJobMaxItems 任务表容量上限（防长时间运行无界增长，超限先清过期再生效）。
const videoJobMaxItems = 4096

var videoJobs = &videoJobRegistry{items: map[string]videoJobEntry{}}

func (v *videoJobRegistry) put(jobID, uid, channel string) {
	if jobID == "" {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.evictLocked()
	v.items[jobID] = videoJobEntry{uid: uid, channel: channel, at: time.Now()}
}

func (v *videoJobRegistry) get(jobID string) (uid, channel string, ok bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	e, found := v.items[jobID]
	if !found || time.Since(e.at) > videoJobTTL {
		return "", "", false
	}
	return e.uid, e.channel, true
}

// evictLocked 清过期项；仍超容量则整体重开（宁可丢旧任务也不无界增长）。
// 调用方需持锁。
func (v *videoJobRegistry) evictLocked() {
	now := time.Now()
	for k, e := range v.items {
		if now.Sub(e.at) > videoJobTTL {
			delete(v.items, k)
		}
	}
	if len(v.items) >= videoJobMaxItems {
		v.items = map[string]videoJobEntry{}
	}
}

// ---------------------------------------------------------------------------
// 共用：读体 / 选号 / 错误
// ---------------------------------------------------------------------------

// readMediaBody 读取并校验媒体请求体，超限/读失败直接写响应并返回 ok=false。
func (h *Handler) readMediaBody(w http.ResponseWriter, r *http.Request, kind string, limit int64) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return nil, false
	}
	if int64(len(body)) > limit {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_body_too_large",
			fmt.Sprintf("%s 请求体超过 %d KB 上限", kind, limit>>10))
		return nil, false
	}
	return body, true
}

// runMedia 在账号池上轮转执行一次媒体调用，成功返回结果。
// 返回的 err 为已分类的 *upstream.Error（或本地调度错误），由调用方映射成响应。
//
// 与 chat 轮转的差异（有意为之）：
//   - 不做会话粘性（媒体请求无多轮语义）；
//   - 失败只换号 + 喂连败计数，**不喂熔断/软冷却**：媒体调用低频，
//     一次生图失败不代表账号 chat 不可用，不应影响 chat 侧的选号排序。
func (h *Handler) runMedia(w http.ResponseWriter, r *http.Request, realm string,
	call func(ctx context.Context, a *auth.Auth) (any, error)) (any, error) {

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.cfg.Pool.PickExcludingForRealm(tried, "", realm)
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		if !h.cfg.Pool.Acquire(acct.UID) {
			continue
		}

		// token 临近过期先刷新（与 chat 同口径）。
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				h.cfg.Pool.NoteFailures(acct.UID)
				h.cfg.Pool.Release(acct.UID)
				if !rotateBackoff(i, r.Context()) {
					break
				}
				continue
			}
			_ = acct.SaveAtomic()
		}

		ctx, cancel := context.WithTimeout(r.Context(), mediaTimeout)
		res, err := call(ctx, acct)
		cancel()

		if err == nil {
			h.cfg.Pool.Release(acct.UID)
			h.cfg.Pool.NoteSuccess(acct.UID)
			return res, nil
		}
		lastErr = err
		h.cfg.Pool.NoteFailures(acct.UID)
		h.cfg.Pool.Release(acct.UID)

		var uerr *upstream.Error
		if errors.As(err, &uerr) {
			if _, ok := mediaHardStopCodes[uerr.Kind]; ok {
				break
			}
		}
		if !rotateBackoff(i, r.Context()) {
			break
		}
	}
	if lastErr == nil {
		lastErr = errNoMediaAccount
	}
	return nil, lastErr
}

// errNoMediaAccount 池中无可用账号时的本地调度错误。
var errNoMediaAccount = errors.New("no healthy account available")

// mediaHardStopCodes 换号无意义的错误种类：请求本身的问题，重试只会浪费健康号。
var mediaHardStopCodes = map[upstream.ErrKind]struct{}{
	upstream.ErrBadParams:     {},
	upstream.ErrPromptTooLong: {},
	upstream.ErrContentBlocked: {},
	upstream.ErrHardCredit:    {},
}

// writeMediaError 把上游/本地错误映射成 OpenAI 风格错误响应。
// 上游原文优先透传（含 code/msg/requestId），本地错误用自有文案。
func (h *Handler) writeMediaError(w http.ResponseWriter, err error) {
	if errors.Is(err, errNoMediaAccount) {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account",
			"all accounts are temporarily unavailable, please retry later")
		return
	}
	var uerr *upstream.Error
	if errors.As(err, &uerr) {
		status := uerr.Status
		if status == 0 || status >= 500 {
			status = http.StatusBadGateway
		}
		msg := strings.TrimSpace(uerr.Msg)
		if msg == "" {
			msg = err.Error()
		}
		writeOpenAIError(w, status, "upstream_error", msg)
		return
	}
	writeOpenAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
}

// fetchAsBase64 回源下载图片并 base64 编码（response_format=b64_json 用）。
func fetchAsBase64(ctx context.Context, url string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, imageDownloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("download status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// parseVideoModel 把生视频模型规格拆成 (vendor, model)。
//
// 支持的写法（厂商前缀大小写不敏感）：
//   - "kling-v2.6"        → ("Kling", "v2.6")     —— /v1/models 目录里的规范 ID
//   - "miora-minimax-h3"  → ("Miora", "minimax-h3")
//   - "kling"             → ("Kling", "")         —— 只给厂商，版本回落上游默认
//   - "miora"             → ("Miora", "")
//   - "v2.6"              → ("", "v2.6")          —— 只给版本，厂商回落默认（Kling）
//   - ""                  → ("", "")              —— 全部回落默认（上游侧补默认）
//
// 厂商清单取自 upstream.VideoVendors()（Kling / Miora），避免两处硬编码漂移。
func parseVideoModel(spec string) (vendor, model string) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return "", ""
	}
	lower := strings.ToLower(s)
	for _, v := range upstream.VideoVendors() {
		prefix := strings.ToLower(v) + "-"
		if strings.HasPrefix(lower, prefix) {
			return v, s[len(prefix):]
		}
		if lower == strings.ToLower(v) {
			return v, ""
		}
	}
	// 无厂商前缀：视为裸版本号，厂商回落默认。
	return "", s
}

// mediaModelEntries 返回多模态（生图/生视频）模型目录条目。
//
// 为何是网关侧静态登记而非上游动态拉取：这两类模型由 images / tcproxy 端点驱动，
// 不在上游 chat 模型列表里；静态登记让客户端可从 /v1/models 发现可用模型名。
// ID 统一走 cn: 前缀协议，与 resolveModel 对称（剥离前缀后即上游裸名/目录 ID）。
// 清单取自 upstream.MediaModelCatalog，与面板展示共用同一份，避免两侧漂移。
func mediaModelEntries() []map[string]any {
	const created = 1753600000
	catalog := upstream.MediaModelCatalog()
	out := make([]map[string]any, 0, len(catalog))
	for _, m := range catalog {
		out = append(out, map[string]any{
			"id":          "cn:" + m.ID,
			"object":      "model",
			"created":     created,
			"owned_by":    "workbuddy",
			"type":        m.Type,
			"capability":  m.Capability,
			"description": m.Description,
		})
	}
	return out
}
