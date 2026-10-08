package panel

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// mkAuthJSON 造一份最小的合法凭证。auth.Parse 只强制要求 accessToken，
// 其余字段可省；补 refreshToken/uid/nickname 让用例更接近真实文件。
func mkAuthJSON(uid string) []byte {
	return []byte(`{"accessToken":"tok-` + uid + `","refreshToken":"ref-` + uid +
		`","uid":"` + uid + `","nickname":"nick-` + uid + `","expiresAt":4102444800}`)
}

// buildZip 按「条目名 → 内容」造 zip；条目名排序保证结果可复现。
func buildZip(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fw, err := zw.Create(n)
		if err != nil {
			t.Fatalf("zip create %s: %v", n, err)
		}
		if _, err := fw.Write(entries[n]); err != nil {
			t.Fatalf("zip write %s: %v", n, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// ---- zip-slip 防护：safeZipRel 是唯一把「包内条目名」变成「落盘相对路径」的地方，
// 它放行的每一个名字都必须落在白名单目录内，绝不能以 .. 或绝对路径逃逸。

func TestSafeZipRel(t *testing.T) {
	okCases := map[string]string{
		"config.json":                "config.json",
		"MIGRATE.txt":                "MIGRATE.txt",
		"auths/workbuddy-abc.json":   "auths/workbuddy-abc.json",
		"data/state.json":            "data/state.json",
		`auths\workbuddy-abc.json`:   "auths/workbuddy-abc.json", // PowerShell 写反斜杠的兼容
		"./auths/workbuddy-abc.json": "auths/workbuddy-abc.json",
	}
	for in, want := range okCases {
		got, ok := safeZipRel(in)
		if !ok || got != want {
			t.Errorf("safeZipRel(%q) = (%q, %v), want (%q, true)", in, got, ok, want)
		}
	}

	bad := []string{
		"",
		"/etc/passwd",
		"../evil.json",
		"auths/../../evil.json",
		`..\..\evil.json`,
		"auths/../config.json",
		"C:/windows/system32/x.json",
		"C:\\windows\\x.json",
		"auths/",
		"auths//x.json",
		"auths/./x.json",
		"other/x.json",     // 非白名单目录
		"auths/a/b/c.json", // 层级过深
		"..",
		".",
	}
	for _, in := range bad {
		if got, ok := safeZipRel(in); ok {
			t.Errorf("safeZipRel(%q) = (%q, true), want rejected", in, got)
		}
	}
}

// ---- readPoolZip：包解析与整包校验

func TestReadPoolZipAcceptsValidPackage(t *testing.T) {
	zipRaw := buildZip(t, map[string][]byte{
		"auths/workbuddy-a.json": mkAuthJSON("a"),
		"auths/workbuddy-b.json": mkAuthJSON("b"),
		"data/state.json":        []byte(`{"accounts":{"a":{"credits":7,"disabled":false}}}`),
		"config.json":            []byte(`{"api_key":"k"}`),
		"MIGRATE.txt":            []byte("说明"),
	})

	plan, ignored, err := readPoolZip(zipRaw, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := plan["auths/workbuddy-a.json"]; !ok {
		t.Error("missing auths/workbuddy-a.json in plan")
	}
	if _, ok := plan["auths/workbuddy-b.json"]; !ok {
		t.Error("missing auths/workbuddy-b.json in plan")
	}
	if _, ok := plan["data/state.json"]; !ok {
		t.Error("missing data/state.json in plan")
	}
	// config 未勾选时必须忽略；MIGRATE.txt 始终忽略 → 共 2 条被忽略。
	if _, ok := plan["config.json"]; ok {
		t.Error("config.json must be ignored when withConfig=false")
	}
	if ignored != 2 {
		t.Errorf("ignored = %d, want 2 (config.json + MIGRATE.txt)", ignored)
	}
}

func TestReadPoolZipHonorsConfigFlag(t *testing.T) {
	zipRaw := buildZip(t, map[string][]byte{
		"auths/workbuddy-a.json": mkAuthJSON("a"),
		"config.json":            []byte(`{"api_key":"k"}`),
	})
	plan, _, err := readPoolZip(zipRaw, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := plan["config.json"]; !ok {
		t.Error("config.json must be included when withConfig=true")
	}
}

func TestReadPoolZipIgnoresUsageJSON(t *testing.T) {
	zipRaw := buildZip(t, map[string][]byte{
		"auths/workbuddy-a.json": mkAuthJSON("a"),
		"data/usage.json":        []byte(`{"total":1}`), // 运行期改写会被 recorder 覆盖，导入必须忽略
	})
	plan, _, err := readPoolZip(zipRaw, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := plan["data/usage.json"]; ok {
		t.Error("usage.json must never be imported")
	}
}

func TestReadPoolZipRejectsNonZip(t *testing.T) {
	if _, _, err := readPoolZip([]byte("this is not a zip"), false); err == nil {
		t.Error("non-zip input must be rejected")
	}
}

func TestReadPoolZipRejectsPackageWithoutAccountsOrState(t *testing.T) {
	zipRaw := buildZip(t, map[string][]byte{
		"MIGRATE.txt":     []byte("说明"),
		"data/usage.json": []byte(`{}`),
	})
	_, _, err := readPoolZip(zipRaw, true)
	if err == nil {
		t.Fatal("package with no accounts and no state.json must be rejected")
	}
	if !strings.Contains(err.Error(), "没有账号凭证") {
		t.Errorf("error should explain the package has no accounts, got: %v", err)
	}
}

func TestReadPoolZipRejectsCorruptAuth(t *testing.T) {
	zipRaw := buildZip(t, map[string][]byte{
		"auths/workbuddy-broken.json": []byte(`{"accessToken":""}`), // Parse 会拒绝
	})
	_, _, err := readPoolZip(zipRaw, false)
	if err == nil {
		t.Fatal("package with unparseable credential must be rejected as a whole")
	}
}

func TestReadPoolZipRejectsZipSlipEntries(t *testing.T) {
	// 恶意包：正常账号 + 一个试图逃逸的条目。逃逸条目应被丢弃（计入 ignored），
	// 且绝不进入 plan。
	zipRaw := buildZip(t, map[string][]byte{
		"auths/workbuddy-a.json": mkAuthJSON("a"),
		"../../evil.json":        []byte(`{"pwned":true}`),
		"/etc/passwd":            []byte("root:x:0:0"),
	})
	plan, ignored, err := readPoolZip(zipRaw, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for name := range plan {
		if strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
			t.Errorf("escaping entry leaked into plan: %q", name)
		}
	}
	if ignored != 2 {
		t.Errorf("ignored = %d, want 2 escaping entries", ignored)
	}
	if len(plan) != 1 {
		t.Errorf("plan size = %d, want 1 (only the valid account)", len(plan))
	}
}

func TestReadPoolZipRejectsTooManyEntries(t *testing.T) {
	entries := map[string][]byte{"auths/workbuddy-a.json": mkAuthJSON("a")}
	for i := 0; i < poolZipMaxEntries+10; i++ {
		entries["data/junk-"+strconv.Itoa(i)+".txt"] = []byte("x")
	}
	zipRaw := buildZip(t, entries)
	_, _, err := readPoolZip(zipRaw, false)
	if err == nil || !strings.Contains(err.Error(), "条目过多") {
		t.Fatalf("zip with too many entries must be rejected, got err=%v", err)
	}
}

// ---- 导出 → 导入 往返：面板导出的包必须能被自己读回来（与离线脚本的兼容性由此保证）

func TestWritePoolZipRoundTrip(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(authDir, "workbuddy-"+uid+".json"), mkAuthJSON(uid), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dataDir, "state.json"), []byte(`{"accounts":{"a":{"credits":3}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"api_key":"k"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	p := New(Config{AuthDir: authDir, StateFile: filepath.Join(dataDir, "state.json"), ConfigPath: cfgPath})

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	n, err := p.writePoolZip(zw)
	if err != nil {
		t.Fatalf("writePoolZip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if n < 3 {
		t.Errorf("written entries = %d, want >= 3", n)
	}

	plan, _, err := readPoolZip(buf.Bytes(), true)
	if err != nil {
		t.Fatalf("exported zip must be importable, got: %v", err)
	}
	for _, want := range []string{"auths/workbuddy-a.json", "auths/workbuddy-b.json", "data/state.json", "config.json"} {
		if _, ok := plan[want]; !ok {
			t.Errorf("round-trip lost %s", want)
		}
	}
	// 凭证必须逐字节不变。
	if !bytes.Equal(plan["auths/workbuddy-a.json"], mkAuthJSON("a")) {
		t.Error("credential bytes changed across export/import round-trip")
	}
}

// ---- 路由接线：导出接口必须挂上鉴权、只接受 GET、并回一个可解析的 zip
// （走真实 mux 与中间件，覆盖 ServeHTTP 这一层，而不只是 handler 函数本身）

func TestPoolExportRoute(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "workbuddy-a.json"), mkAuthJSON("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(Config{Version: "test", APIKey: "k", AuthDir: authDir})

	// 无密钥 → 401（且不能漏出任何包内容）。
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/pool/export", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: code=%d want 401", rec.Code)
	}

	// 带密钥 → 200 + zip 附件头。
	req := httptest.NewRequest("GET", "/panel/api/pool/export", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("Content-Type=%q want application/zip", ct)
	}
	dispo := rec.Header().Get("Content-Disposition")
	if !strings.Contains(dispo, "attachment") || !strings.Contains(dispo, "pool-") {
		t.Errorf("Content-Disposition=%q want attachment with a pool-*.zip name", dispo)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control=%q want no-store (the zip carries plaintext credentials)", cc)
	}

	// 必须回一个能再次导入的合法 zip。
	plan, _, err := readPoolZip(rec.Body.Bytes(), true)
	if err != nil {
		t.Fatalf("exported body is not a valid pool zip: %v", err)
	}
	if _, ok := plan["auths/workbuddy-a.json"]; !ok {
		t.Error("exported zip is missing the account credential")
	}
}

// ---- 端到端：HTTP 导入真的替换磁盘凭证、写 state.json、备份旧池、并让进程内池换新

func TestPoolImportEndToEnd(t *testing.T) {
	root := t.TempDir()
	authDir := filepath.Join(root, "auths")
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stateFp := filepath.Join(dataDir, "state.json")
	cfgPath := filepath.Join(root, "config.json")

	// 本机现状：1 个账号（会作为导入前备份的内容）。
	oldAuth := filepath.Join(authDir, "workbuddy-old.json")
	if err := os.WriteFile(oldAuth, mkAuthJSON("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldCfgs, _ := auth.LoadDir(authDir)
	pl := pool.New(stateFp)
	defer pl.Close()
	pl.ReplaceFromState(nil, oldCfgs)
	if got := len(pl.List()); got != 1 {
		t.Fatalf("setup: pool size = %d, want 1", got)
	}

	p := New(Config{
		Version:    "test",
		APIKey:     "test-key",
		Pool:       pl,
		AuthDir:    authDir,
		StateFile:  stateFp,
		ConfigPath: cfgPath,
	})

	// 导入包：2 个新账号 + 一份标记 state（a 被禁用），不覆盖 config。
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	part, err := mw.CreateFormFile("package", "pool.zip")
	if err != nil {
		t.Fatal(err)
	}
	zipRaw := buildZip(t, map[string][]byte{
		"auths/workbuddy-a.json": mkAuthJSON("a"),
		"auths/workbuddy-b.json": mkAuthJSON("b"),
		// cool_kind 是数值枚举（CoolKind int），此处不带即为 0=无冷却。
		"data/state.json": []byte(`{"accounts":{"a":{"credits":42,"disabled":true,"reason":"test"}}}`),
		"config.json":     []byte(`{"api_key":"SHOULD-NOT-APPLY"}`),
	})
	if _, err := part.Write(zipRaw); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	// backups/ 相对工作目录，切到 root 让备份落进临时目录。
	cwd, _ := os.Getwd()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)

	req := httptest.NewRequest("POST", "/panel/api/pool/import", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK             bool   `json:"ok"`
		Accounts       int    `json:"accounts"`
		AccountsBefore int    `json:"accounts_before"`
		StateImported  bool   `json:"state_imported"`
		StateApplied   bool   `json:"state_applied"`
		ConfigImported bool   `json:"config_imported"`
		Backup         string `json:"backup"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response json: %v (%s)", err, rec.Body.String())
	}
	if !resp.OK || resp.Accounts != 2 || resp.AccountsBefore != 1 {
		t.Fatalf("unexpected outcome: %+v", resp)
	}
	if !resp.StateImported {
		t.Error("state_imported = false, want true")
	}
	if !resp.StateApplied {
		t.Error("state_applied = false, want true (a valid state.json must reach the pool)")
	}
	if resp.ConfigImported {
		t.Error("config_imported = true, but the checkbox was off")
	}

	// 磁盘：旧账号被清掉，新账号就位。
	names, _ := filepath.Glob(filepath.Join(authDir, "workbuddy*.json"))
	var bases []string
	for _, n := range names {
		bases = append(bases, filepath.Base(n))
	}
	sort.Strings(bases)
	want := []string{"workbuddy-a.json", "workbuddy-b.json"}
	if strings.Join(bases, ",") != strings.Join(want, ",") {
		t.Errorf("auths dir = %v, want %v", bases, want)
	}
	if _, err := os.Stat(oldAuth); !os.IsNotExist(err) {
		t.Error("old credential must be removed by a replacing import")
	}

	// config.json 不该被写。
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Error("config.json must not be written when the checkbox is off")
	}

	// 进程内池已换新，且 state 里的禁用标记生效（热加载确实吃到了字节）。
	got := pl.List()
	if len(got) != 2 {
		t.Fatalf("pool size after import = %d, want 2", len(got))
	}
	var found bool
	for _, s := range got {
		if s.UID == "a" {
			found = true
			if !s.Disabled {
				t.Error("imported state.json was not applied: account a should be disabled")
			}
			if s.Credits != 42 {
				t.Errorf("account a credits = %d, want 42", s.Credits)
			}
		}
	}
	if !found {
		t.Error("imported account a missing from pool")
	}

	// 备份目录里有旧凭证（可回滚）。
	if resp.Backup == "" {
		t.Fatal("backup dir not reported")
	}
	if _, err := os.Stat(filepath.Join(resp.Backup, "auths", "workbuddy-old.json")); err != nil {
		t.Errorf("backup missing old credential: %v", err)
	}
}

// 包内 state.json 解析不了时：账号照常导入，但必须如实回报 state_applied=false，
// 而不是含糊地当成成功（否则用户会以为积分/冷却状态也搬过来了）。
func TestPoolImportReportsUnparseableState(t *testing.T) {
	root := t.TempDir()
	authDir := filepath.Join(root, "auths")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pl := pool.New("")
	defer pl.Close()

	p := New(Config{Version: "test", APIKey: "k", Pool: pl, AuthDir: authDir})

	zipRaw := buildZip(t, map[string][]byte{
		"auths/workbuddy-a.json": mkAuthJSON("a"),
		// accounts 是 map，塞字符串进去必然解析失败。
		"data/state.json": []byte(`{"accounts":"not-an-object"}`),
	})
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	part, _ := mw.CreateFormFile("package", "pool.zip")
	_, _ = part.Write(zipRaw)
	_ = mw.Close()

	cwd, _ := os.Getwd()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)

	req := httptest.NewRequest("POST", "/panel/api/pool/import", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK           bool `json:"ok"`
		Accounts     int  `json:"accounts"`
		StateApplied bool `json:"state_applied"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Accounts != 1 {
		t.Fatalf("accounts must still import: %+v", resp)
	}
	if resp.StateApplied {
		t.Error("state_applied = true, but the state.json was unparseable")
	}
}

// ---- 导入必须为「替换」语义：包内没有的本机账号要被清掉

func TestPoolImportReplacesRatherThanMerges(t *testing.T) {
	root := t.TempDir()
	authDir := filepath.Join(root, "auths")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 本机 2 个账号，包里只有 1 个。
	for _, uid := range []string{"keep-me-gone", "keep-me-too-gone"} {
		if err := os.WriteFile(filepath.Join(authDir, "workbuddy-"+uid+".json"), mkAuthJSON(uid), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	olds, _ := auth.LoadDir(authDir)
	pl := pool.New("")
	defer pl.Close()
	pl.ReplaceFromState(nil, olds)

	p := New(Config{Version: "test", APIKey: "k", Pool: pl, AuthDir: authDir})

	zipRaw := buildZip(t, map[string][]byte{"auths/workbuddy-only.json": mkAuthJSON("only")})
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	part, _ := mw.CreateFormFile("package", "pool.zip")
	_, _ = part.Write(zipRaw)
	_ = mw.Close()

	cwd, _ := os.Getwd()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)

	req := httptest.NewRequest("POST", "/panel/api/pool/import", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	names, _ := filepath.Glob(filepath.Join(authDir, "workbuddy*.json"))
	if len(names) != 1 || filepath.Base(names[0]) != "workbuddy-only.json" {
		t.Fatalf("auths after import = %v, want only workbuddy-only.json", names)
	}
	if got := len(pl.List()); got != 1 {
		t.Errorf("pool size = %d, want 1 (import replaces, not merges)", got)
	}
}
