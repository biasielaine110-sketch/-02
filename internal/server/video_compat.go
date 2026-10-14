// video_compat.go OpenAI Videos API 兼容层。
//
// 面向按 **OpenAI 视频协议**调用的前端（如「Nww-无限画布」infinite-atelier）：
//
//	POST /v1/videos       提交任务 → {id, object:"video", status:"queued", …}
//	GET  /v1/videos/{id}  轮询任务 → 完成后在 video_url / url 给 mp4 直链
//
// 与网关原生端点的关系：**同一条提交/查询管线**（kling tcproxy / miora H3），
// 只是把入参形态与状态词表翻译成 OpenAI 风格；原生 /v1/videos/generations 不受影响。
//
// 入参兼容两种编码（该前端两种都会发）：
//   - application/json    {model,prompt,seconds,duration,ratio,aspect_ratio,size,resolution[,input_reference]}
//   - multipart/form-data 同上字段 + 可重复的 input_reference 文件（参考图，最多 7 张）
//
// input_reference 是 OpenAI 视频协议的参考图字段，本层映射到 miora H3 的图生视频：
// 单张 → first_frame（首帧，frame_to_video）；多张 → references（多参考，reference_to_video）。
// Kling（tcproxy）不支持参考图，由 submitVideoJob 的既有闸门直接 400——与原生端点同口径，
// 绝不静默忽略让调用方误以为图生视频生效。
//
// 状态词表：网关三态（running/succeeded/failed）→ OpenAI（in_progress/completed/failed）。
// 前端只在 status=="completed" 时取片、在 failed/cancelled 时抛错，其余一律继续轮询。
package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// videosCompatCreate 处理 OpenAI 风格的视频提交 POST /v1/videos。
func (h *Handler) videosCompatCreate(w http.ResponseWriter, r *http.Request) {
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))

	var (
		req videoRequest
		err error
	)
	if mediaType == "multipart/form-data" {
		req, err = parseVideoCompatMultipart(w, r)
	} else {
		var body []byte
		var ok bool
		if body, ok = h.readMediaBody(w, r, "video", videoMaxBodyBytes); !ok {
			return
		}
		req, err = parseVideoCompatJSON(body)
	}
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	jobID, ok := h.submitVideoJob(w, r, req)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       jobID,
		"object":   "video",
		"created":  time.Now().Unix(),
		"status":   "queued",
		"model":    req.Model,
		"progress": 0,
	})
}

// videosCompatGet 处理 OpenAI 风格的视频轮询 GET /v1/videos/{id}。
func (h *Handler) videosCompatGet(w http.ResponseWriter, r *http.Request) {
	jobID := strings.TrimSpace(r.PathValue("id"))
	if jobID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "job id is required")
		return
	}
	job, ok := h.queryVideoJob(w, r, jobID)
	if !ok {
		return
	}

	status, progress := compatVideoStatus(job)
	out := map[string]any{
		"id":              job.JobID,
		"object":          "video",
		"status":          status,
		"progress":        progress,
		"upstream_status": job.Status,
	}
	if job.ResultURL != "" {
		// 前端依次尝试 video_url / result_url / url / content.*，任一命中即视为完成。
		out["video_url"] = job.ResultURL
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

// compatVideoStatus 把网关三态翻成 OpenAI 视频状态词表与进度百分比。
func compatVideoStatus(job *upstream.VideoJob) (string, int) {
	switch {
	case job.Done():
		return "completed", 100
	case job.Failed():
		return "failed", 100
	default:
		return "in_progress", 50
	}
}

// ---------------------------------------------------------------------------
// 入参解析
// ---------------------------------------------------------------------------

// compatVideoJSON OpenAI 视频提交的 JSON 形态。该前端还会多发 seconds/duration、
// ratio/aspect_ratio/size、resolution/resolution_name 等同义别名，这里统一收敛到 videoRequest。
type compatVideoJSON struct {
	Model          string          `json:"model"`
	Prompt         string          `json:"prompt"`
	Duration       looseSeconds    `json:"duration"`
	Seconds        looseSeconds    `json:"seconds"`
	AspectRatio    string          `json:"aspect_ratio"`
	Ratio          string          `json:"ratio"`
	Size           string          `json:"size"`
	Resolution     string          `json:"resolution"`
	ResolutionName string          `json:"resolution_name"`
	InputReference json.RawMessage `json:"input_reference"`
}

// looseSeconds 接受数字或字符串（前端两种都会发）形式的秒数；非数值按「未提供」处理。
type looseSeconds int

func (s *looseSeconds) UnmarshalJSON(b []byte) error {
	t := strings.TrimSpace(strings.Trim(strings.TrimSpace(string(b)), `"`))
	if t == "" || t == "null" {
		*s = 0
		return nil
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		*s = 0
		return nil
	}
	*s = looseSeconds(int(f + 0.5))
	return nil
}

// parseVideoCompatJSON 把 OpenAI 视频 JSON 体翻译成网关内部 videoRequest。
func parseVideoCompatJSON(body []byte) (videoRequest, error) {
	var c compatVideoJSON
	if err := json.Unmarshal(body, &c); err != nil {
		return videoRequest{}, fmt.Errorf("parse request: %w", err)
	}
	req := videoRequest{
		Model:       strings.TrimSpace(c.Model),
		Prompt:      c.Prompt,
		Duration: firstPositiveInt(int(c.Duration), int(c.Seconds)),
		AspectRatio: normalizeAspectRatio(firstNonEmpty(c.AspectRatio, c.Ratio, ratioLike(c.Size))),
		Resolution:  normalizeVideoResolution(firstNonEmpty(c.Resolution, c.ResolutionName)),
	}
	imgs, err := parseInputReference(c.InputReference)
	if err != nil {
		return videoRequest{}, err
	}
	applyReferenceImages(&req, imgs)
	return req, nil
}

// parseVideoCompatMultipart 解析 multipart/form-data 形态的提交（带 input_reference 文件）。
func parseVideoCompatMultipart(w http.ResponseWriter, r *http.Request) (videoRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, videoMaxBodyBytes)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		return videoRequest{}, fmt.Errorf("parse multipart: %w", err)
	}
	get := func(k string) string { return strings.TrimSpace(r.FormValue(k)) }

	req := videoRequest{
		Model:       get("model"),
		Prompt:      get("prompt"),
		Duration: firstPositiveInt(parseLooseInt(get("duration")), parseLooseInt(get("seconds"))),
		AspectRatio: normalizeAspectRatio(firstNonEmpty(get("aspect_ratio"), get("ratio"), ratioLike(get("size")))),
		Resolution:  normalizeVideoResolution(firstNonEmpty(get("resolution"), get("resolution_name"))),
	}
	imgs, err := readMultipartReferenceImages(r)
	if err != nil {
		return videoRequest{}, err
	}
	applyReferenceImages(&req, imgs)
	return req, nil
}

// readMultipartReferenceImages 把 multipart 里的 input_reference 文件读成 base64 载荷。
// 该前端在「有参考图」时走 multipart，把图片原始字节作为 input_reference 直接上传。
func readMultipartReferenceImages(r *http.Request) ([]upstream.MediaPayload, error) {
	if r.MultipartForm == nil {
		return nil, nil
	}
	files := r.MultipartForm.File["input_reference"]
	if len(files) == 0 {
		return nil, nil
	}
	out := make([]upstream.MediaPayload, 0, len(files))
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			return nil, fmt.Errorf("open input_reference: %w", err)
		}
		raw, err := io.ReadAll(io.LimitReader(f, 32<<20))
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("read input_reference: %w", err)
		}
		if len(raw) == 0 {
			continue
		}
		out = append(out, upstream.MediaPayload{Base64: base64.StdEncoding.EncodeToString(raw)})
	}
	return out, nil
}

// parseInputReference 解析 OpenAI 的 input_reference 字段，兼容：
//   - 缺省 / null                 → 无参考图（文生）
//   - 单个字符串 / 对象            → 一张
//   - 字符串 / 对象数组            → 多张
//
// 元素形态：URL 字符串、data URI、裸 base64，或 {image_url|url|base64|file_key} 对象。
func parseInputReference(raw json.RawMessage) ([]upstream.MediaPayload, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	var items []json.RawMessage
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, fmt.Errorf("input_reference: %w", err)
		}
	} else {
		items = []json.RawMessage{trimmed}
	}
	out := make([]upstream.MediaPayload, 0, len(items))
	for _, it := range items {
		p, err := parseOneMediaRef(it)
		if err != nil {
			return nil, fmt.Errorf("input_reference: %w", err)
		}
		if p != nil {
			out = append(out, *p)
		}
	}
	return out, nil
}

// parseOneMediaRef 解析单个参考图元素，返回 nil 表示该元素为空（跳过）。
func parseOneMediaRef(raw json.RawMessage) (*upstream.MediaPayload, error) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return nil, nil
	}
	// 对象形态：先认 OpenAI 的 image_url 写法，再交给 MediaPayload 处理 url/base64/file_key。
	if t[0] == '{' {
		var probe struct {
			ImageURL string `json:"image_url"`
		}
		if err := json.Unmarshal(t, &probe); err == nil && strings.TrimSpace(probe.ImageURL) != "" {
			return &upstream.MediaPayload{URL: strings.TrimSpace(probe.ImageURL)}, nil
		}
	}
	var p upstream.MediaPayload
	if err := json.Unmarshal(t, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// applyReferenceImages 把参考图映射到 miora 的图生视频入参：
// 单张 → 首帧（frame_to_video）；多张 → 多参考（reference_to_video）。
func applyReferenceImages(req *videoRequest, imgs []upstream.MediaPayload) {
	switch {
	case len(imgs) == 1:
		first := imgs[0]
		req.FirstFrame = &first
	case len(imgs) > 1:
		refs := imgs
		req.References = &refs
	}
}

// ---------------------------------------------------------------------------
// 归一化小工具
// ---------------------------------------------------------------------------

// compatAspectAliases 把常见但不在 miora 白名单内的画幅映射到最近的受支持值。
// miora 白名单：adaptive / 21:9 / 16:9 / 4:3 / 1:1 / 3:4 / 9:16。
var compatAspectAliases = map[string]string{
	"2:3":   "3:4",
	"3:2":   "4:3",
	"4:5":   "3:4",
	"5:4":   "4:3",
	"16:10": "16:9",
	"10:16": "9:16",
}

var ratioPattern = regexp.MustCompile(`^\d{1,2}:\d{1,2}$`)

// normalizeAspectRatio 归一画幅：命中别名表则改写，其余原样透出（miora 侧仍会校验）。
func normalizeAspectRatio(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if v, ok := compatAspectAliases[strings.ToLower(s)]; ok {
		return v
	}
	return s
}

// normalizeVideoResolution 把常见分辨率写法收敛到 miora 白名单词表（768P / 2K）。
// Kling 通道不消费该字段，故此处不做通道区分；未知写法原样透出交由上游校验。
func normalizeVideoResolution(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return ""
	case "720", "720p", "768", "768p", "hd":
		return "768P"
	case "1080", "1080p", "1k", "2k", "1440", "1440p", "2160", "2160p", "4k", "uhd":
		return "2K"
	default:
		return strings.TrimSpace(s)
	}
}

// ratioLike 只在 s 形如 "16:9" 时返回它，避免把像素尺寸（"1280x720"）当画幅。
func ratioLike(s string) string {
	s = strings.TrimSpace(s)
	if ratioPattern.MatchString(s) {
		return s
	}
	return ""
}

// firstNonEmpty 返回首个非空（去空白后）字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// firstPositiveInt 返回首个正数（<=0 视为未提供）。
func firstPositiveInt(vals ...int) int {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

// parseLooseInt 宽松解析（multipart 表单值都是字符串，可能带小数）。
func parseLooseInt(s string) int {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return int(f + 0.5)
}
