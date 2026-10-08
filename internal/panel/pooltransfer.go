// pooltransfer.go 面板内的账号池导入/导出（跨机器迁移）。
//
// 与仓库里的离线脚本（scripts/pool-transfer.ps1 + export-pool.bat / import-pool.bat）
// 目标一致，且产物互相兼容：面板导出的 zip 能被 import-pool.bat 导入，离线脚本导出的
// zip 也能被面板导入——两边都只认 auths/ + data/ + config.json 这几个位置。
//
// 三个设计决定（都是被本项目的运行方式逼出来的，改动前先读）：
//
//  1. 导入是「替换」而不是「合并」。先清掉 auths/workbuddy*.json，再写入包内账号。
//     合并会让本机上已有的、包内没有的账号留下来，迁移语义变成"两份池子搅在一起"，
//     而不是"把这台机器变成那台机器"。替换前整份备份到 backups/pre-import-<时间戳>/，
//     可回滚。
//
//  2. 导入后在进程内热替换账号池（pool.ReplaceFromState）。state.json 的内容直接以
//     字节喂进池，写盘由池自己在持锁后完成——否则 5s 一次的 flusher 会在"文件已换、
//     内存未换"的窗口里用旧状态把导入结果覆盖掉。
//
//  3. usage.json 不参与导入。用量记录器常驻内存且自带 30s 防抖落盘，运行期改写该文件
//     随后就会被它写回，导入等于白做。导出仍然打包它（离线脚本与手工恢复用得上），
//     导入时明确忽略并回报，不含糊其辞。
package panel

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	// poolZipMaxBytes 导入包体积上限。真实包约 40 KB（14 个账号 + 状态），8 MB 留足余量。
	poolZipMaxBytes = 8 << 20
	// poolZipMaxEntries 解压条目数上限：挡住"几万个小条目"型的 zip 炸弹。
	poolZipMaxEntries = 4096
	// poolBackupRoot 导入前备份的根目录（与离线脚本同一位置，便于统一回滚）。
	poolBackupRoot = "backups"
)

// authFilePattern 与 auth.LoadDir 的扫描口径严格一致：只认 workbuddy*.json。
// 导出、备份、清理三处都用它，保证"池子认哪些文件"只有一个定义。
const authFilePattern = "workbuddy*.json"

// ---------------------------------------------------------------------------
// 导出
// ---------------------------------------------------------------------------

// poolExport 把当前账号池打包成 zip 直接回给浏览器下载。
//
// 打包内容与离线脚本一致：auths/*.json + data/*.json + config.json + MIGRATE.txt。
// 缺哪个就少哪个（例如全新机器还没有 state.json），不因此报错。
func (p *Panel) poolExport(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	n, err := p.writePoolZip(zw)
	if err == nil {
		err = zw.Close()
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "打包失败: "+err.Error())
		return
	}
	name := "pool-" + time.Now().Format("20060102-150405") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
	log.Printf("panel: 账号池已导出 %s（%d 个条目, %.1f KB）", name, n, float64(buf.Len())/1024)
}

// writePoolZip 把账号池写进 zip，返回写入条目数。
func (p *Panel) writePoolZip(zw *zip.Writer) (int, error) {
	n := 0

	// auths/*.json —— 主体，账号凭证。
	if p.cfg.AuthDir != "" {
		names, _ := filepath.Glob(filepath.Join(p.cfg.AuthDir, authFilePattern))
		sort.Strings(names)
		for _, fp := range names {
			raw, err := os.ReadFile(fp)
			if err != nil {
				continue // 打包期间被删/无权限：跳过，不让整包失败
			}
			if err := zipAdd(zw, "auths/"+filepath.Base(fp), raw); err != nil {
				return n, err
			}
			n++
		}
	}

	// data/*.json —— 账号池运行状态（state.json）与用量（usage.json）等。
	if dir := p.dataDir(); dir != "" {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			if err := zipAdd(zw, "data/"+e.Name(), raw); err != nil {
				return n, err
			}
			n++
		}
	}

	// config.json —— 网关配置（含门禁密钥）。
	if p.cfg.ConfigPath != "" {
		if raw, err := os.ReadFile(p.cfg.ConfigPath); err == nil {
			if err := zipAdd(zw, "config.json", raw); err != nil {
				return n, err
			}
			n++
		}
	}

	// MIGRATE.txt —— 仓库里的迁移说明，随包分发，让收包的人有据可依。
	if raw, err := os.ReadFile(filepath.Join("scripts", "MIGRATE.txt")); err == nil {
		if err := zipAdd(zw, "MIGRATE.txt", raw); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// zipAdd 写一个 zip 条目。
//
// 显式把分隔符统一成 '/'：PowerShell 5.1 的 Compress-Archive 会写反斜杠，导致
// Linux/macOS 解压出 "auths\xxx.json" 这种含反斜杠的怪文件名。Go 侧不能重复那个坑。
func zipAdd(zw *zip.Writer, name string, data []byte) error {
	fw, err := zw.Create(strings.ReplaceAll(name, `\`, "/"))
	if err != nil {
		return err
	}
	_, err = fw.Write(data)
	return err
}

// dataDir 由 state 文件路径推出数据目录（与 main 的 stateSibling 同规则）。
func (p *Panel) dataDir() string {
	if p.cfg.StateFile == "" {
		return ""
	}
	return filepath.Dir(p.cfg.StateFile)
}

// ---------------------------------------------------------------------------
// 导入
// ---------------------------------------------------------------------------

// poolImport 接收上传的账号池 zip 并应用：备份 → 替换 auths/ 与 data/state.json
// →（可选）写 config.json → 进程内热替换账号池。
//
// 表单字段：
//
//	package  file  必填，账号池 zip
//	config   "1"   可选，是否连 config.json 一起覆盖（默认不覆盖，见下方说明）
//
// 为什么不默认覆盖 config.json：它装的是网关门禁密钥。运行期把它换掉会出现
// "磁盘上是新密钥、进程里还是旧密钥"的割裂状态，而且下次保存配置又会被覆盖回去；
// 对正在用面板的人来说，突然换掉自己的门禁密钥是个很容易把自己关在门外的动作。
// 所以要显式勾选，并且提示需要重启才真正生效。
func (p *Panel) poolImport(w http.ResponseWriter, r *http.Request) {
	// 读上传（体积硬上限，超限由 MaxBytesReader 直接截断并报错）。
	r.Body = http.MaxBytesReader(w, r.Body, poolZipMaxBytes)
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "读取上传失败（可能超出 "+strconv.Itoa(poolZipMaxBytes>>20)+" MB 上限）: "+err.Error())
		return
	}
	f, _, err := r.FormFile("package")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "上传内容里没有找到压缩包字段 package")
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取压缩包内容失败: "+err.Error())
		return
	}
	withConfig := r.FormValue("config") == "1"

	// 解析 + 逐条校验（含 zip-slip 防护），拿到"要写到哪、写什么"。
	plan, ignored, err := readPoolZip(raw, withConfig)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	src, err := p.applyPoolPlan(plan)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "导入失败: "+err.Error())
		return
	}

	resp := map[string]any{
		"ok":              true,
		"accounts":        src.accounts,
		"accounts_before": src.accountsBefore,
		"state_imported":  src.stateImported,
		"state_applied":   src.stateApplied,
		"config_imported": src.configImported,
		"ignored":         ignored,
		"backup":          src.backup,
	}
	if src.configImported {
		resp["restart_required"] = []string{"config"}
	}
	writeJSON(w, http.StatusOK, resp)
}

// importOutcome 导入结果的摘要（用于回报与日志）。
type importOutcome struct {
	accounts       int
	accountsBefore int
	stateImported  bool // 包内带了 state.json 且已落盘
	stateApplied   bool // 该 state.json 解析成功并已作用到进程内池（false = 包坏了或没带）
	configImported bool
	backup         string
}

// applyPoolPlan 执行一次导入的落盘动作，并让进程内账号池跟着换新。
//
// 顺序是刻意的：先备份 → 再改文件 → 最后热替换池。任何一步失败都直接返回，
// 且此时磁盘要么是原样、要么已有备份可回滚。
func (p *Panel) applyPoolPlan(plan map[string][]byte) (importOutcome, error) {
	var out importOutcome

	if p.cfg.Pool == nil {
		return out, errors.New("账号池不可用")
	}
	out.accountsBefore = len(p.cfg.Pool.List())

	// 1. 备份当前账号池（失败即中止：没有回滚点不该动用户数据）。
	backup, err := p.backupPoolBeforeImport()
	if err != nil {
		return out, errors.New("备份现有账号池失败，已中止导入: " + err.Error())
	}
	out.backup = backup

	// 2. 替换 auths/：先清掉本机旧账号，再写入包内账号。
	//    只删 authFilePattern 匹配的文件，目录里其他东西不动。
	if p.cfg.AuthDir != "" {
		if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
			return out, err
		}
		old, _ := filepath.Glob(filepath.Join(p.cfg.AuthDir, authFilePattern))
		for _, fp := range old {
			if err := os.Remove(fp); err != nil && !os.IsNotExist(err) {
				return out, errors.New("清理旧凭证失败 " + filepath.Base(fp) + ": " + err.Error())
			}
		}
		names := make([]string, 0, len(plan))
		for rel := range plan {
			if strings.HasPrefix(rel, "auths/") {
				names = append(names, rel)
			}
		}
		sort.Strings(names)
		for _, rel := range names {
			if err := writeFileAtomic(filepath.Join(p.cfg.AuthDir, filepath.Base(rel)), plan[rel]); err != nil {
				return out, errors.New("写入 " + rel + " 失败: " + err.Error())
			}
		}
	}

	// 3. state.json：文件写一份（供重启/离线工具读取），内容同时喂给池做热替换。
	if raw, ok := plan["data/state.json"]; ok && p.cfg.StateFile != "" {
		if err := writeFileAtomic(p.cfg.StateFile, raw); err != nil {
			return out, errors.New("写入 state.json 失败: " + err.Error())
		}
		out.stateImported = true
	}

	// 4. config.json（仅在显式勾选时）。
	if raw, ok := plan["config.json"]; ok && p.cfg.ConfigPath != "" {
		if err := writeFileAtomic(p.cfg.ConfigPath, raw); err != nil {
			return out, errors.New("写入 config.json 失败: " + err.Error())
		}
		out.configImported = true
	}

	// 5. 重新扫描 auths/ 并整体替换池状态。ReplaceFromState 的返回值区分「没带 state」
	//    与「带了但解析不了」——后者说明用户拿到的包是坏的，必须如实回报而不是含糊成成功。
	accounts, err := auth.LoadDir(p.cfg.AuthDir)
	if err != nil {
		return out, errors.New("重新加载凭证目录失败: " + err.Error())
	}
	out.stateApplied = p.cfg.Pool.ReplaceFromState(plan["data/state.json"], accounts)
	out.accounts = len(p.cfg.Pool.List())

	log.Printf("panel: 账号池导入完成（%d → %d 个账号, state=%v/%v, config=%v, 备份=%s）",
		out.accountsBefore, out.accounts, out.stateImported, out.stateApplied, out.configImported, backup)
	return out, nil
}

// readPoolZip 解析并校验导入包，返回「相对路径 → 内容」以及被忽略的条目数。
//
// 只接受三个目标位置，其余条目一律忽略（包内可以带 MIGRATE.txt 之类的说明文件）：
//
//	auths/workbuddy*.json  账号凭证
//	data/state.json        账号池运行状态
//	config.json            网关门禁配置（仅当 withConfig 为真）
//
// 必须至少有一个账号或一份 state.json，否则认为传错了文件（例如把日志包传上来）。
func readPoolZip(raw []byte, withConfig bool) (map[string][]byte, int, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, 0, errors.New("不是有效的 zip 文件: " + err.Error())
	}
	if len(zr.File) > poolZipMaxEntries {
		return nil, 0, fmt.Errorf("包内条目过多（%d > %d），已拒绝", len(zr.File), poolZipMaxEntries)
	}

	out := map[string][]byte{}
	ignored := 0
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rel, ok := safeZipRel(f.Name)
		if !ok {
			ignored++
			continue
		}
		switch {
		case strings.HasPrefix(rel, "auths/") && filepath.Base(rel) != "" &&
			strings.HasPrefix(filepath.Base(rel), "workbuddy") && strings.HasSuffix(rel, ".json"):
			// 账号凭证，收。
		case rel == "data/state.json":
			// 账号池状态，收。
		case rel == "config.json":
			if !withConfig {
				ignored++
				continue
			}
		default:
			ignored++ // usage.json / MIGRATE.txt / 其它一概忽略
			continue
		}

		rc, err := f.Open()
		if err != nil {
			return nil, 0, fmt.Errorf("打开条目 %s 失败: %w", f.Name, err)
		}
		content, err := io.ReadAll(io.LimitReader(rc, poolZipMaxBytes))
		_ = rc.Close()
		if err != nil {
			return nil, 0, fmt.Errorf("读取条目 %s 失败: %w", f.Name, err)
		}
		out[rel] = content
	}

	// 账号凭证要能解析、state.json 要能解析成 JSON，否则整包拒绝——
	// 半坏的包应用下去比不应用更糟（会出现"导入成功但一半账号是废的"）。
	nAcct := 0
	for rel, content := range out {
		if strings.HasPrefix(rel, "auths/") {
			if _, err := auth.Parse(content); err != nil {
				return nil, 0, fmt.Errorf("包内凭证 %s 无法解析（包可能损坏）: %v", rel, err)
			}
			nAcct++
		}
	}
	if nAcct == 0 && len(out["data/state.json"]) == 0 {
		return nil, 0, errors.New("包内没有账号凭证或账号池状态，确认一下是不是选错了 zip")
	}
	return out, ignored, nil
}

// safeZipRel 把 zip 条目名规范成安全的相对路径；不安全或超出白名单目录范围返回 false。
//
// zip-slip 防护的落点：统一分隔符 → 拒绝绝对路径 / 盘符 / 空段 / "." / ".." →
// 只允许根级单段文件与 auths|x/data|x 的二段路径。调用方再按 basename 落盘，
// 写出的路径必然落在目标目录内。
func safeZipRel(name string) (string, bool) {
	s := strings.ReplaceAll(name, `\`, "/")
	s = strings.TrimPrefix(s, "./")
	if s == "" || strings.HasPrefix(s, "/") || strings.Contains(s, ":") {
		return "", false
	}
	parts := strings.Split(s, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return "", false
		}
	}
	switch len(parts) {
	case 1:
		return parts[0], true // config.json / MIGRATE.txt 等根级文件
	case 2:
		if parts[0] == "auths" || parts[0] == "data" {
			return parts[0] + "/" + parts[1], true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// backupPoolBeforeImport 把当前账号池整份备份到 backups/pre-import-<时间戳>/。
// 返回备份目录。空池（新机器首次导入）算成功，不报错。
func (p *Panel) backupPoolBeforeImport() (string, error) {
	dir := filepath.Join(poolBackupRoot, "pre-import-"+time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	copied := 0

	if p.cfg.AuthDir != "" {
		names, _ := filepath.Glob(filepath.Join(p.cfg.AuthDir, authFilePattern))
		for _, fp := range names {
			if err := copyFile(fp, filepath.Join(dir, "auths", filepath.Base(fp))); err != nil {
				return "", err
			}
			copied++
		}
	}
	for _, item := range []struct{ src, sub string }{
		{p.cfg.StateFile, "data"},
		{p.cfg.ConfigPath, ""},
	} {
		if item.src == "" {
			continue
		}
		dst := filepath.Join(dir, item.sub, filepath.Base(item.src))
		if err := copyFile(item.src, dst); err != nil {
			if os.IsNotExist(err) {
				continue // 还没生成过，不算失败
			}
			return "", err
		}
		copied++
	}
	log.Printf("panel: 导入前已备份账号池 %d 个文件 → %s", copied, dir)
	return dir, nil
}

// copyFile 复制文件，权限收成 0600（含明文凭证）。
func copyFile(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeFileAtomic(dst, raw)
}

// writeFileAtomic 原子写：先写同目录临时文件再 rename，避免读到写了一半的文件。
// 凭证与状态文件都含敏感信息，权限统一 0600。
func writeFileAtomic(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
