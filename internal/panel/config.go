// config.go 面板配置页接口：读取当前配置、校验并保存（热生效 + 重启项标注）。
//
// 分工：cmd/server 持有 Config 类型与校验逻辑（Load/normalize），此处只做
// HTTP 编排——GET 回显、POST 透传给注入的 SaveConfig 闭包（由 main 完成
// "校验 → 落盘 → 热应用 → 返回需重启字段列表"）。
package panel

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
)

// getConfig 返回用于面板表单的配置副本与路径。密钥只允许写入或轮换，绝不经
// 读取接口回显，以免 API key、上游设备令牌或外部存储令牌进入浏览器存储和日志。
func (p *Panel) getConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	cfg, err := p.cfg.LoadConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load config: "+err.Error())
		return
	}
	view, err := redactConfig(cfg)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "encode config view: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"path":   p.cfg.ConfigPath,
		"config": view,
	})
}

// redactConfig creates an independent JSON-compatible view and removes fields
// whose names identify secrets. Keeping this generic prevents new config
// secrets from being accidentally exposed before the panel schema is updated.
func redactConfig(cfg any) (any, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var view any
	if err := json.Unmarshal(raw, &view); err != nil {
		return nil, err
	}
	redactConfigValue(view)
	return view, nil
}

func redactConfigValue(v any) {
	switch value := v.(type) {
	case map[string]any:
		for key, child := range value {
			if isSensitiveConfigKey(key) {
				delete(value, key)
				continue
			}
			redactConfigValue(child)
		}
	case []any:
		for _, child := range value {
			redactConfigValue(child)
		}
	}
}

func isSensitiveConfigKey(key string) bool {
	key = strings.ToLower(key)
	return strings.Contains(key, "token") || strings.Contains(key, "secret") ||
		strings.Contains(key, "password") || key == "api_key" || key == "apikey"
}

// saveConfig 保存配置：body 直接是配置 JSON（前端按 schema 组装完整对象）。
// SaveConfig 闭包内部完成校验+落盘+热应用；校验失败返回 400 且不写盘。
func (p *Panel) saveConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	restartRequired, err := p.cfg.SaveConfig(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if restartRequired == nil {
		restartRequired = []string{}
	}
	log.Printf("panel: 配置已保存（热生效完成；需重启字段 %d 个）", len(restartRequired))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"restart_required": restartRequired,
	})
}
