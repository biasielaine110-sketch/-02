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
	"math"
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
	// 参考图若是「本网关 /v1/files/{id}」的回链（画布上传后原样回填），直接物化成
	// base64 送上游——省掉上游回源抓我们域名的一跳，也免了 Funnel 抖动导致的假失败。
	h.materializeVideoRefs(&req)

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
//
// 参考图有两套字段名都会出现：
//   - input_reference：OpenAI 视频协议（`{image_url:…}` 或字符串/数组）；
//   - first_frame_image / last_frame_image：miora 原生叫法，画布的**第三套兜底请求体**
//     直接用它。少认这一对就会把「图生视频」静默降级成文生视频（画布首尾两套失败后
//     回落到这里，图被丢掉却仍然 200 出片——最难查的那种错）。
type compatVideoJSON struct {
	Model           string          `json:"model"`
	Prompt          string          `json:"prompt"`
	Duration        looseSeconds    `json:"duration"`
	Seconds         looseSeconds    `json:"seconds"`
	AspectRatio     string          `json:"aspect_ratio"`
	Ratio           string          `json:"ratio"`
	Size            string          `json:"size"`
	Resolution      string          `json:"resolution"`
	ResolutionName  string          `json:"resolution_name"`
	InputReference  json.RawMessage `json:"input_reference"`
	FirstFrameImage json.RawMessage `json:"first_frame_image"`
	LastFrameImage  json.RawMessage `json:"last_frame_image"`
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
		Duration:    firstPositiveInt(int(c.Duration), int(c.Seconds)),
		AspectRatio: normalizeAspectRatio(firstNonEmpty(c.AspectRatio, c.Ratio, ratioLike(c.Size))),
		Resolution:  normalizeVideoResolution(firstNonEmpty(c.Resolution, c.ResolutionName, resolutionLike(c.Size))),
	}
	imgs, err := parseInputReference(c.InputReference)
	if err != nil {
		return videoRequest{}, err
	}
	applyReferenceImages(&req, imgs)
	// first_frame_image / last_frame_image：miora 原生字段名。已由 input_reference
	// 给出首帧时不覆盖（前者更贴近 OpenAI 协议，优先级更高）。
	if req.FirstFrame == nil {
		p, err := parseOneMediaRef(c.FirstFrameImage)
		if err != nil {
			return videoRequest{}, fmt.Errorf("first_frame_image: %w", err)
		}
		req.FirstFrame = p
	}
	if req.LastFrame == nil {
		p, err := parseOneMediaRef(c.LastFrameImage)
		if err != nil {
			return videoRequest{}, fmt.Errorf("last_frame_image: %w", err)
		}
		req.LastFrame = p
	}
	return req, nil
}

// multipart 里的图片字段名。参考图用前三者，首/尾帧用后两者（画布的不同兜底形态
// 会用不同叫法；少认一个就静默丢图）。
var (
	multipartRefFields   = []string{"input_reference", "images", "image", "references"}
	multipartFirstFields = []string{"first_frame_image", "first_frame"}
	multipartLastFields  = []string{"last_frame_image", "last_frame"}
)

// parseVideoCompatMultipart 解析 multipart/form-data 形态的提交（带参考图文件/字段）。
func parseVideoCompatMultipart(w http.ResponseWriter, r *http.Request) (videoRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, videoMaxBodyBytes)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		return videoRequest{}, fmt.Errorf("parse multipart: %w", err)
	}
	get := func(k string) string { return strings.TrimSpace(r.FormValue(k)) }

	req := videoRequest{
		Model:       get("model"),
		Prompt:      get("prompt"),
		Duration:    firstPositiveInt(parseLooseInt(get("duration")), parseLooseInt(get("seconds"))),
		AspectRatio: normalizeAspectRatio(firstNonEmpty(get("aspect_ratio"), get("ratio"), ratioLike(get("size")))),
		Resolution:  normalizeVideoResolution(firstNonEmpty(get("resolution"), get("resolution_name"), resolutionLike(get("size")))),
	}

	// 首/尾帧：先找文件字段，再退到表单值（URL 或 base64 都可能直接作为字段值发来）。
	p, err := firstMultipartMedia(r, multipartFirstFields)
	if err != nil {
		return videoRequest{}, fmt.Errorf("first_frame_image: %w", err)
	}
	req.FirstFrame = p
	p, err = firstMultipartMedia(r, multipartLastFields)
	if err != nil {
		return videoRequest{}, fmt.Errorf("last_frame_image: %w", err)
	}
	req.LastFrame = p

	// 没显式给首/尾帧时，才把 input_reference/images 当参考图（1 张→首帧，多张→多参考）。
	if req.FirstFrame == nil && req.LastFrame == nil {
		imgs, err := readMultipartMedia(r, multipartRefFields)
		if err != nil {
			return videoRequest{}, err
		}
		applyReferenceImages(&req, imgs)
	}
	return req, nil
}

// firstMultipartMedia 按字段名依次找一张图：先文件字段，再表单值。都没有返回 nil。
func firstMultipartMedia(r *http.Request, names []string) (*upstream.MediaPayload, error) {
	imgs, err := readMultipartMedia(r, names)
	if err != nil {
		return nil, err
	}
	if len(imgs) > 0 {
		return &imgs[0], nil
	}
	for _, n := range names {
		if p := mediaFromFormValue(r.FormValue(n)); p != nil {
			return p, nil
		}
	}
	return nil, nil
}

// readMultipartMedia 把指定字段名下的文件读成 base64 载荷（按 names 顺序、组内按出现顺序）。
// 该前端在「有参考图」时走 multipart，把图片原始字节作为字段文件上传。
func readMultipartMedia(r *http.Request, names []string) ([]upstream.MediaPayload, error) {
	if r.MultipartForm == nil {
		return nil, nil
	}
	var out []upstream.MediaPayload
	for _, name := range names {
		for _, fh := range r.MultipartForm.File[name] {
			f, err := fh.Open()
			if err != nil {
				return nil, fmt.Errorf("open %s: %w", name, err)
			}
			raw, err := io.ReadAll(io.LimitReader(f, 32<<20))
			_ = f.Close()
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", name, err)
			}
			if len(raw) == 0 {
				continue
			}
			out = append(out, upstream.MediaPayload{Base64: base64.StdEncoding.EncodeToString(raw)})
		}
	}
	return out, nil
}

// mediaFromFormValue 把 multipart 表单值当图片载荷：http(s) 视为 URL，其余交给
// MediaPayload 的字符串简写（data URI / 裸 base64）。空值返回 nil。
func mediaFromFormValue(s string) *upstream.MediaPayload {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return &upstream.MediaPayload{URL: s}
	}
	return &upstream.MediaPayload{Base64: s}
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

// pixelSizePattern 匹配 "1280x720" / "768×1344" / "1024 * 1024" 这类像素尺寸。
// 画布（infinite-atelier）按像素尺寸下发视频 size，故 size 必须能反推画幅与分辨率档。
var pixelSizePattern = regexp.MustCompile(`^(\d{2,5})\s*[xX×*]\s*(\d{2,5})$`)

// aspectCandidates 把像素尺寸就近映射到 miora 画幅白名单（与 compatAspectAliases 目标一致）。
var aspectCandidates = []struct {
	name string
	val  float64
}{
	{"21:9", 21.0 / 9.0},
	{"16:9", 16.0 / 9.0},
	{"4:3", 4.0 / 3.0},
	{"1:1", 1.0},
	{"3:4", 3.0 / 4.0},
	{"9:16", 9.0 / 16.0},
}

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

// ratioLike 把请求里的 size 字段归一成画幅：
//   - "16:9" 这类冒号比例 → 原样返回；
//   - "768x1344" / "720x1280" 这类**像素尺寸** → 就近映射到白名单画幅。
//
// 画布按像素尺寸下发 size（如竖屏 768x1344），早先只认冒号写法会让画幅被静默丢弃、
// 上游退化成默认横屏（实测 size-only → 1344×768）。这里仍**绝不**把像素串直接当画幅透传，
// 一律收敛到 miora 白名单词表后再给上游。
func ratioLike(s string) string {
	s = strings.TrimSpace(s)
	if ratioPattern.MatchString(s) {
		return s
	}
	if w, h, ok := parsePixelSize(s); ok {
		return nearestAspectRatio(float64(w) / float64(h))
	}
	return ""
}

// resolutionLike 从像素 size 反推 miora 分辨率档（768P / 2K）；非像素尺寸返回空串。
// 短边 < 1000 → 768P，≥ 1000 → 2K（对应画布 SD/HD 两档：768x1344 vs 1440x2560）。
// 仅作 resolution/resolution_name 缺省时的兜底，不覆盖客户端显式给的档位。
func resolutionLike(s string) string {
	w, h, ok := parsePixelSize(strings.TrimSpace(s))
	if !ok {
		return ""
	}
	short := w
	if h < short {
		short = h
	}
	if short >= 1000 {
		return "2K"
	}
	return "768P"
}

// parsePixelSize 解析 "768x1344"（认 x / X / × / * / 空格分隔）为宽高。
func parsePixelSize(s string) (int, int, bool) {
	m := pixelSizePattern.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, false
	}
	w, err1 := strconv.Atoi(m[1])
	h, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// nearestAspectRatio 在 aspectCandidates 里取与给定宽高比 log 距离最近者。
func nearestAspectRatio(ar float64) string {
	best := ""
	bestDist := math.MaxFloat64
	for _, c := range aspectCandidates {
		if d := math.Abs(math.Log(ar / c.val)); d < bestDist {
			bestDist, best = d, c.name
		}
	}
	return best
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
