// image_compat.go OpenAI Images Edits 兼容层。
//
// 面向按 **OpenAI 图片编辑协议**调用的前端（如「Nww-无限画布」infinite-atelier 的
// POST /v1/images/edits）：multipart 或 JSON 给「一张/多张参考图 + 指令」，拿回改图结果。
//
//	POST /v1/images/edits → {created, data:[{url}]}（response_format=b64_json 时给 b64_json）
//
// 上游现状：网关原有的生图通道（混元 /v2/images/generations，action=SubmitTextToImageJob）
// **只有文生图，没有编辑能力**；真正能做「给图改图」的是 miora 通道的
// /api/ai/workbuddy-proxy/image/edit/submit（实测放行）。故本层固定走 miora 改图。
//
// 入参兼容两种编码（对应 OpenAI 的两种调用方式，前端两种都会发）：
//   - multipart/form-data：model / prompt / n / size / response_format + 可重复的 image 文件
//   - application/json   ：同上字段 + images:[…]（也接受单数 image / image_url）
//
// 参考图元素形态与视频一致：URL 字符串、data URI、裸 base64，或
// {url|base64|file_key|image_url} 对象（见 parseInputReference / upstream.MediaPayload）。
package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// imagesEditsCompat 处理 OpenAI 风格的改图 POST /v1/images/edits。
func (h *Handler) imagesEditsCompat(w http.ResponseWriter, r *http.Request) {
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))

	var (
		req    imageRequest
		images []upstream.MediaPayload
		err    error
	)
	if mediaType == "multipart/form-data" {
		req, images, err = parseImagesEditMultipart(w, r)
	} else {
		var body []byte
		var ok bool
		if body, ok = h.readMediaBody(w, r, "image", imageUploadMaxBodyBytes); !ok {
			return
		}
		req, images, err = parseImagesEditJSON(body)
	}
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// 前端发的是比例（1:1 / 16:9 …）或 auto，上游只认「数字x数字」——先归一。
	req.Size = normalizeImageSize(req.Size)
	if strings.TrimSpace(req.Prompt) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "prompt is required")
		return
	}
	if len(images) == 0 {
		// 没有参考图就不是「改图」了。上游对空 images 会回 MISSING_INPUT，本地先拦一道。
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"at least one reference image is required (multipart `image` file, or JSON `images`)")
		return
	}
	if req.N > 1 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"n greater than 1 is not supported by cn:miora-image* models")
		return
	}

	realm, bare := resolveModel(req.Model)
	// 改图只有 miora 通道能做；未指定模型时默认走 miora 改图。
	if strings.TrimSpace(bare) == "" {
		bare = upstream.MioraImageEditModel
	}
	if !isMioraImageSpec(bare) {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"image edits are only supported by cn:"+upstream.MioraImageModel+"* models (got "+bare+")")
		return
	}

	res, uerr := h.runMediaWithTimeout(w, r, realm, mioraImageTimeout,
		func(ctx context.Context, a *auth.Auth) (any, error) {
			list := images
			return h.cfg.Upstream.SubmitImageMiora(ctx, a, upstream.MioraImageRequest{
				Prompt:  req.Prompt,
				Size:    req.Size,
				Quality: req.Quality,
				Images:  &list,
			})
		})
	if uerr != nil {
		h.writeMediaError(w, uerr)
		return
	}
	ir := res.(*upstream.MioraImageResult)
	h.writeImagesResponse(w, r, []upstream.ImageResult{{URL: ir.URL}}, req.ResponseFormat, upstream.MediaUsage{})
}

// ---------------------------------------------------------------------------
// 入参解析
// ---------------------------------------------------------------------------

// compatImagesEditJSON OpenAI 改图的 JSON 形态。
// n 用 looseSeconds 宽松取数（数字/字符串都收，与视频入口同口径）。
type compatImagesEditJSON struct {
	Model          string          `json:"model"`
	Prompt         string          `json:"prompt"`
	N              looseSeconds    `json:"n"`
	Size           string          `json:"size"`
	Quality        string          `json:"quality"`
	ResponseFormat string          `json:"response_format"`
	Images         json.RawMessage `json:"images"`
	Image          json.RawMessage `json:"image"`
	ImageURL       json.RawMessage `json:"image_url"`
	Mask           json.RawMessage `json:"mask"`
}

// parseImagesEditJSON 把 OpenAI 改图 JSON 体翻译成网关内部入参。
func parseImagesEditJSON(body []byte) (imageRequest, []upstream.MediaPayload, error) {
	var c compatImagesEditJSON
	if err := json.Unmarshal(body, &c); err != nil {
		return imageRequest{}, nil, fmt.Errorf("parse request: %w", err)
	}
	if len(strings.TrimSpace(string(c.Mask))) > 0 && strings.TrimSpace(string(c.Mask)) != "null" {
		// 上游没有遮罩概念。静默忽略会让调用方以为只有局部被改，故明确拒绝。
		return imageRequest{}, nil, fmt.Errorf("mask is not supported by cn:%s* models", upstream.MioraImageModel)
	}
	req := imageRequest{
		Model:          strings.TrimSpace(c.Model),
		Prompt:         c.Prompt,
		N:              int(c.N),
		Size:           strings.TrimSpace(c.Size),
		Quality:        strings.TrimSpace(c.Quality),
		ResponseFormat: strings.TrimSpace(c.ResponseFormat),
	}
	imgs, err := mergeMediaRefs(c.Images, c.Image, c.ImageURL)
	if err != nil {
		return imageRequest{}, nil, err
	}
	return req, imgs, nil
}

// parseImagesEditMultipart 解析 multipart 形态的改图（参考图作为文件上传）。
func parseImagesEditMultipart(w http.ResponseWriter, r *http.Request) (imageRequest, []upstream.MediaPayload, error) {
	r.Body = http.MaxBytesReader(w, r.Body, imageUploadMaxBodyBytes)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		return imageRequest{}, nil, fmt.Errorf("parse multipart: %w", err)
	}
	get := func(k string) string { return strings.TrimSpace(r.FormValue(k)) }

	if get("mask") != "" {
		return imageRequest{}, nil, fmt.Errorf("mask is not supported by cn:%s* models", upstream.MioraImageModel)
	}
	req := imageRequest{
		Model:          get("model"),
		Prompt:         get("prompt"),
		N:              parseLooseInt(get("n")),
		Size:           get("size"),
		Quality:        get("quality"),
		ResponseFormat: get("response_format"),
	}
	imgs, err := readMultipartImages(r)
	if err != nil {
		return imageRequest{}, nil, err
	}
	return req, imgs, nil
}

// imageMultipartKeys 参考图可能出现的文件字段名（OpenAI 是 image，部分前端用 images/image[]）。
var imageMultipartKeys = []string{"image", "images", "image[]"}

// readMultipartImages 读出 multipart 里的参考图：文件字段当原始字节转 base64，
// 同名普通表单项（如把 URL 填进 image）按 URL/data URI/裸 base64 解析。
func readMultipartImages(r *http.Request) ([]upstream.MediaPayload, error) {
	if r.MultipartForm == nil {
		return nil, nil
	}
	out := make([]upstream.MediaPayload, 0, 4)
	for _, key := range imageMultipartKeys {
		for _, fh := range r.MultipartForm.File[key] {
			f, err := fh.Open()
			if err != nil {
				return nil, fmt.Errorf("open %s: %w", key, err)
			}
			raw, err := io.ReadAll(io.LimitReader(f, 32<<20))
			_ = f.Close()
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", key, err)
			}
			if len(raw) == 0 {
				continue
			}
			out = append(out, upstream.MediaPayload{Base64: base64.StdEncoding.EncodeToString(raw)})
		}
		for _, v := range r.MultipartForm.Value[key] {
			if strings.TrimSpace(v) == "" {
				continue
			}
			var p upstream.MediaPayload
			if err := json.Unmarshal([]byte(strconv.Quote(strings.TrimSpace(v))), &p); err != nil {
				return nil, fmt.Errorf("%s: %w", key, err)
			}
			out = append(out, p)
		}
	}
	return out, nil
}

// mergeMediaRefs 合并 images / image / image_url 三处参考图输入（任一可缺省）。
func mergeMediaRefs(raws ...json.RawMessage) ([]upstream.MediaPayload, error) {
	out := make([]upstream.MediaPayload, 0, 4)
	for _, raw := range raws {
		imgs, err := parseInputReference(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, imgs...)
	}
	return out, nil
}

// isMioraImageSpec 报告模型规格是否指向 miora 图片通道（含裸厂商名 "miora"）。
func isMioraImageSpec(spec string) bool {
	s := strings.TrimSpace(spec)
	return upstream.IsMioraImageModel(s) || strings.EqualFold(s, upstream.MioraVideoVendor)
}

// ---------------------------------------------------------------------------
// size 归一（比例 → 像素）
// ---------------------------------------------------------------------------

// pxSizePattern 上游接受的尺寸写法：`数字x数字`（宽松认 × / * / X 与空格）。
var pxSizePattern = regexp.MustCompile(`^\d{1,5}\s*[xX×*]\s*\d{1,5}$`)

// pxSizeSeparator 像素尺寸里的分隔符（连同两侧空白），统一改写成小写 x。
var pxSizeSeparator = regexp.MustCompile(`\s*[xX×*]\s*`)

// imageSizeAliases 把前端常见的**比例**写法映射到上游要求的像素尺寸。
//
// 起因（实测）：上游只认 `数字x数字`，收到 `1:1` / `16:9` / `auto` 一律回
// 400 `size must be in format "数字x数字"`。而按 OpenAI 协议调用的前端
// （如「Nww-无限画布」的图像尺寸选择器）发的**正是比例**，照原样转发必然失败。
// 这里做一次忠实的翻译（比例 → 最接近的像素尺寸），不是降级：画幅语义保持不变。
var imageSizeAliases = map[string]string{
	"1:1":  "1024x1024",
	"4:3":  "1024x768",
	"3:4":  "768x1024",
	"3:2":  "1152x768",
	"2:3":  "768x1152",
	"16:9": "1280x720",
	"9:16": "720x1280",
	"21:9": "1344x576",
}

// normalizeImageSize 放宽生图 / 改图的 size 入参：
//   - 空 / `auto`                → ""（丢弃，让上游用默认尺寸；OpenAI 的 auto 就是这个语义）
//   - `数字x数字`（含 × / * / 空格）→ 归一为小写 x 后透出
//   - 常见比例（1:1 / 16:9 …）     → 映射到最接近的像素尺寸
//   - 其它                       → 原样透出，交由上游给出清晰报错（不静默改写，避免画幅被悄悄换掉）
func normalizeImageSize(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "auto") {
		return ""
	}
	if pxSizePattern.MatchString(s) {
		return pxSizeSeparator.ReplaceAllString(s, "x")
	}
	if v, ok := imageSizeAliases[strings.ToLower(s)]; ok {
		return v
	}
	return s
}
