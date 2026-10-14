// files.go 参考图上传端点（画布类前端「先把图换成公网链接」那一步）。
//
// 为什么需要它：「Nww-无限画布」的视频链路（其 bundle 里的 nK 函数）在**提交生成之前**
// 必须先把参考图 POST 到 `POST /v1/files/upload`，并拿到一个 https 链接——它硬性校验
// `/^https?:\/\//`，**data URI 不算数**；拿到后把该链接作为
// `input_reference:{image_url:…}` 回填到 `POST /v1/videos`。
//
// 此前网关没有这个端点 → 404 → 画布直接报「视频生成超时，请稍后重试」，
// 而且因为「上传」这一步就断了，网关访问日志里**连视频提交记录都看不到**
// （只有一条 `/api/proxy?target=…/v1/files/upload` 的 404），极易误判成链路问题。
//
// 实现：
//  1. multipart 收下文件 → 存进程内（TTL，见 filestore.go）；
//  2. 回自己的公网 URL `<publicBase>/v1/files/{id}`；
//  3. `GET /v1/files/{id}` **不鉴权**——取图方可能是上游（miora）的图片抓取器，带不了 Bearer；
//  4. 更稳的一手：媒体入参归一里把「指向自己的文件 URL」直接物化成 base64
//     （materializeOwnFiles），于是上游根本不需要回源抓我们的链接。
//
// 同时实现 `POST /v1/uploads/images`——同一套存储，另一种响应外壳。画布的「参考图预处理」
// 会先试它、失败再回落标准改图接口；补上可以让那条路也走通而不是先吃一个 404。
package server

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// uploadMaxBodyBytes 上传请求体上限。画布对 >2.8MB 的参考图会先压缩，视频首帧通常更小；
// 24MB 与改图/视频请求体同档，足够容纳 2K 级图片。
const uploadMaxBodyBytes = 24 << 20

// uploadFieldNames 接受的上传字段名。画布用 "file"（唯一实测），其余为常见客户端形态兜底。
var uploadFieldNames = []string{"file", "image", "images", "input_reference", "first_frame_image", "files[]"}

// filesUpload 处理 `POST /v1/files/upload` 与 `POST /v1/uploads/images`。
//
// 响应同时给出 url / data.url / success 三种外壳，覆盖两类消费方：
//   - 画布 files/upload：读 `data.url || data.data.url`
//   - 画布 uploads/images：读 `data.data.url || data.url`，且 `success===false` 时视为失败
func (h *Handler) filesUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, uploadMaxBodyBytes)
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "parse multipart: "+err.Error())
		return
	}
	data, mimeType, err := readUploadedFile(r)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	id := h.cfg.Files.put(data, mimeType)
	abs := h.fileURL(r, id)
	if abs == "" {
		// 拿不到公网基址（既没配 server.public_base_url，请求也没带 Host/Marker）。
		// 宁可明说不支持，也不回一个假链接让画布拿去提交、再在上游侧神秘失败。
		writeOpenAIError(w, http.StatusInternalServerError, "upload_unavailable",
			"cannot determine public base url for uploaded file; set server.public_base_url")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      id,
		"url":     abs,
		"success": true,
		"data":    map[string]any{"id": id, "url": abs},
	})
}

// filesGet 处理 `GET /v1/files/{id}`：原样吐回上传的字节。
// **故意不鉴权**：上游图片抓取器 / 画布回显都可能直接拉这个链接。
// id 是 128bit 随机串，不可枚举，等价于一条不可猜测的签名链接。
func (h *Handler) filesGet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "file id is required")
		return
	}
	it, ok := h.cfg.Files.get(id)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "uploaded file not found or expired")
		return
	}
	if it.mime != "" {
		w.Header().Set("Content-Type", it.mime)
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("Content-Length", fmt.Sprint(len(it.data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(it.data)
}

// readUploadedFile 从 multipart 表单里取出第一个非空文件，返回字节与 mime。
func readUploadedFile(r *http.Request) ([]byte, string, error) {
	if r.MultipartForm == nil {
		return nil, "", errors.New("multipart form is required")
	}
	for _, name := range uploadFieldNames {
		for _, fh := range r.MultipartForm.File[name] {
			f, err := fh.Open()
			if err != nil {
				return nil, "", fmt.Errorf("open %s: %w", name, err)
			}
			raw, err := io.ReadAll(io.LimitReader(f, uploadMaxBodyBytes))
			_ = f.Close()
			if err != nil {
				return nil, "", fmt.Errorf("read %s: %w", name, err)
			}
			if len(raw) == 0 {
				continue
			}
			return raw, detectUploadMime(fh.Header.Get("Content-Type"), fh.Filename, raw), nil
		}
	}
	return nil, "", errors.New("no file uploaded (expected a multipart file part named file/image/input_reference)")
}

// detectUploadMime 定 mime：先信声明的 Content-Type，缺失/不可辨识时按文件头嗅探，
// 再退到扩展名。上游对纯 base64 载荷并不强制 mime，这里主要是让 GET 回源时头正确。
func detectUploadMime(declared, filename string, raw []byte) string {
	if ct := strings.TrimSpace(strings.Split(declared, ";")[0]); strings.HasPrefix(ct, "image/") {
		return ct
	}
	if sniffed := http.DetectContentType(raw); strings.HasPrefix(sniffed, "image/") {
		return sniffed
	}
	if ext := strings.ToLower(fileExt(filename)); ext != "" {
		if t := mime.TypeByExtension(ext); strings.HasPrefix(t, "image/") {
			return t
		}
	}
	return http.DetectContentType(raw)
}

func fileExt(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return name[i:]
	}
	return ""
}

// fileURL 拼出上传文件的公网地址。
func (h *Handler) fileURL(r *http.Request, id string) string {
	base := h.publicBaseURL(r)
	if base == "" {
		return ""
	}
	return base + "/v1/files/" + id
}

// publicBaseURL 推断本网关对外的基址，优先级：
//  1. 配置 server.public_base_url（最可靠，反向代理改写 Host 时也不受影响）；
//  2. 请求头 X-Forwarded-Proto / X-Forwarded-Host（Tailscale Funnel、Cloudflare 都会带）；
//  3. scheme（TLS 判定）+ r.Host。
func (h *Handler) publicBaseURL(r *http.Request) string {
	if v, ok := h.publicBase.Load().(string); ok && v != "" {
		return v
	}
	if b := strings.TrimRight(strings.TrimSpace(h.cfg.PublicBaseURL), "/"); b != "" {
		return b
	}
	scheme := "http"
	if p := firstHeaderValue(r.Header.Get("X-Forwarded-Proto")); p != "" {
		scheme = strings.ToLower(p)
	} else if r.TLS != nil {
		scheme = "https"
	}
	host := firstHeaderValue(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = strings.TrimSpace(r.Host)
	}
	if host == "" {
		return ""
	}
	return scheme + "://" + host
}

// firstHeaderValue 取「逗号分隔头」的第一个值（X-Forwarded-* 经多跳会串成一串）。
func firstHeaderValue(v string) string {
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// ---------------------------------------------------------------------------
// 把自己的文件 URL 物化成 base64
// ---------------------------------------------------------------------------

// ownFileID 判断 u 是否指向本网关的 `/v1/files/{id}`，返回其中的 id。
// 只看路径，不看主机——主机可能被反向代理改写，而 /v1/files/ 是我们独有的路由。
func ownFileID(u string) (string, bool) {
	p := strings.TrimSpace(u)
	if i := strings.Index(p, "://"); i >= 0 {
		rest := p[i+3:]
		j := strings.IndexByte(rest, '/')
		if j < 0 {
			return "", false
		}
		p = rest[j:]
	}
	const prefix = "/v1/files/"
	if !strings.HasPrefix(p, prefix) {
		return "", false
	}
	id := strings.TrimPrefix(p, prefix)
	if id == "" || strings.ContainsAny(id, "/?#") {
		return "", false
	}
	return id, true
}

// materializeOwnFile 把「指向本网关已上传文件」的 URL 换成内联 base64。
//
// 好处有二：① 上游不必回源抓我们的公网链接（少一跳、少一个失败面）；
// ② 即使 Funnel 抖动 / 上游抓取器对我们的域名有策略限制，也能正常出图。
// 不是自己的 URL、或 id 已过期查不到时，**原样返回**（交给上游按 URL 处理）。
func (h *Handler) materializeOwnFile(p upstream.MediaPayload) upstream.MediaPayload {
	if strings.TrimSpace(p.URL) == "" || h.cfg.Files == nil {
		return p
	}
	id, ok := ownFileID(p.URL)
	if !ok {
		return p
	}
	it, ok := h.cfg.Files.get(id)
	if !ok {
		return p
	}
	return upstream.MediaPayload{Base64: base64.StdEncoding.EncodeToString(it.data)}
}

// materializeVideoRefs 物化视频请求里的首/尾帧与参考图。
func (h *Handler) materializeVideoRefs(req *videoRequest) {
	if req.FirstFrame != nil {
		m := h.materializeOwnFile(*req.FirstFrame)
		req.FirstFrame = &m
	}
	if req.LastFrame != nil {
		m := h.materializeOwnFile(*req.LastFrame)
		req.LastFrame = &m
	}
	if req.References != nil && len(*req.References) > 0 {
		refs := make([]upstream.MediaPayload, 0, len(*req.References))
		for _, p := range *req.References {
			refs = append(refs, h.materializeOwnFile(p))
		}
		req.References = &refs
	}
}

// materializeImageRefs 物化改图请求里的参考图数组。
func (h *Handler) materializeImageRefs(imgs *[]upstream.MediaPayload) {
	if imgs == nil || len(*imgs) == 0 {
		return
	}
	out := make([]upstream.MediaPayload, 0, len(*imgs))
	for _, p := range *imgs {
		out = append(out, h.materializeOwnFile(p))
	}
	*imgs = out
}
