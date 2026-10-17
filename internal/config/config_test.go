package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newLookup 用 map 模拟进程环境变量。
func newLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := LoadFrom(newLookup(nil))
	if err != nil {
		t.Fatalf("默认配置应加载成功，实际报错: %v", err)
	}

	if cfg.HTTPAddr != DefaultHTTPAddr {
		t.Errorf("HTTPAddr = %q，期望 %q", cfg.HTTPAddr, DefaultHTTPAddr)
	}
	if cfg.Env != EnvDev {
		t.Errorf("Env = %q，期望 %q", cfg.Env, EnvDev)
	}
	if !cfg.MCPEnabled {
		t.Error("MCP_ENABLED 默认应为 true")
	}
	if cfg.OpenAPIEnabled {
		t.Error("OPENAPI_ENABLED 默认应为 false")
	}
	if cfg.MaxBodyBytes != DefaultMaxBodyBytes {
		t.Errorf("MaxBodyBytes = %d，期望 %d", cfg.MaxBodyBytes, DefaultMaxBodyBytes)
	}
	if cfg.ExitWaitTime != DefaultExitWaitTime {
		t.Errorf("ExitWaitTime = %s，期望 %s", cfg.ExitWaitTime, DefaultExitWaitTime)
	}
	if cfg.LogFormat != LogFormatJSON || !cfg.IsJSONLog() {
		t.Errorf("LogFormat = %q，期望 json", cfg.LogFormat)
	}
	if len(cfg.Apps) != 1 || cfg.Apps[0].Code != "huobao" {
		t.Fatalf("默认应注册单个 huobao 应用，实际 %+v", cfg.Apps)
	}
	if cfg.Apps[0].BaseURL != DefaultHuobaoBaseURL {
		t.Errorf("huobao 默认下游 = %q，期望 %q", cfg.Apps[0].BaseURL, DefaultHuobaoBaseURL)
	}
	if cfg.IsProduction() {
		t.Error("默认环境不应该是 production")
	}
}

func TestLoadAppsFromEnv(t *testing.T) {
	cfg, err := LoadFrom(newLookup(map[string]string{
		"HUOBAO_BASE_URL": "http://huobao.internal:8081",
		"APPS":            "huobao,novel=http://novel.internal:8082",
	}))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	if len(cfg.Apps) != 2 {
		t.Fatalf("应解析出 2 个应用，实际 %d", len(cfg.Apps))
	}
	// 未显式给出地址的 huobao 应回落到 HUOBAO_BASE_URL。
	if cfg.Apps[0].Code != "huobao" || cfg.Apps[0].BaseURL != "http://huobao.internal:8081" {
		t.Errorf("huobao 解析异常: %+v", cfg.Apps[0])
	}
	if cfg.Apps[1].Code != "novel" || cfg.Apps[1].BaseURL != "http://novel.internal:8082" {
		t.Errorf("novel 解析异常: %+v", cfg.Apps[1])
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	cases := []struct {
		name   string
		env    map[string]string
		expect string
	}{
		{"监听地址缺少端口", map[string]string{"ENTRY_HTTP_ADDR": "127.0.0.1"}, "ENTRY_HTTP_ADDR"},
		{"监听端口非法", map[string]string{"ENTRY_HTTP_ADDR": ":70000"}, "ENTRY_HTTP_ADDR"},
		{"环境取值非法", map[string]string{"ENTRY_ENV": "staging"}, "ENTRY_ENV"},
		{"日志级别非法", map[string]string{"LOG_LEVEL": "verbose"}, "LOG_LEVEL"},
		{"日志格式非法", map[string]string{"LOG_FORMAT": "xml"}, "LOG_FORMAT"},
		{"布尔值非法", map[string]string{"MCP_ENABLED": "maybe"}, "MCP_ENABLED"},
		{"超时非法", map[string]string{"TIMEOUT_READ": "15"}, "TIMEOUT_READ"},
		{"体积非法", map[string]string{"MAX_BODY_BYTES": "-1"}, "MAX_BODY_BYTES"},
		{"应用编码非法", map[string]string{"APPS": "Bad_Code=http://x"}, "应用编码非法"},
		{"应用编码重复", map[string]string{"APPS": "huobao,huobao"}, "重复"},
		{"下游地址非法", map[string]string{"APPS": "huobao=ftp://x"}, "下游地址非法"},
		{"缺少 = 的应用条目", map[string]string{"APPS": "=http://x"}, "缺少应用编码"},
		{"签发方地址非法", map[string]string{"AUTH_ISSUER_BASE_URL": "not-a-url"}, "AUTH_ISSUER_BASE_URL"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadFrom(newLookup(tc.env))
			if err == nil {
				t.Fatalf("期望报错，实际加载成功")
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Errorf("错误信息 %q 未包含 %q", err.Error(), tc.expect)
			}
		})
	}
}

func TestProductionRejectsAuthStub(t *testing.T) {
	_, err := LoadFrom(newLookup(map[string]string{
		"ENTRY_ENV":         EnvProduction,
		"AUTH_STUB_ENABLED": "true",
	}))
	if err == nil {
		t.Fatal("生产环境 + 签发桩必须拒绝启动")
	}
	if !strings.Contains(err.Error(), "AUTH_STUB_ENABLED") {
		t.Errorf("错误信息应指明违规配置，实际: %v", err)
	}

	// 非生产环境允许开启。
	if _, err := LoadFrom(newLookup(map[string]string{"AUTH_STUB_ENABLED": "true"})); err != nil {
		t.Errorf("dev 环境开启签发桩不应报错: %v", err)
	}
}

func TestParseBoolAcceptsCommonForms(t *testing.T) {
	for _, raw := range []string{"1", "true", "TRUE", "yes", "on", "y"} {
		if value, err := parseBool(raw); err != nil || !value {
			t.Errorf("parseBool(%q) = %v, %v，期望 true", raw, value, err)
		}
	}
	for _, raw := range []string{"0", "false", "no", "off", "n"} {
		if value, err := parseBool(raw); err != nil || value {
			t.Errorf("parseBool(%q) = %v, %v，期望 false", raw, value, err)
		}
	}
	if _, err := parseBool("maybe"); err == nil {
		t.Error("无法识别的布尔值应当报错")
	}
}

func TestParseEnvLine(t *testing.T) {
	cases := []struct {
		raw       string
		wantKey   string
		wantValue string
		wantOK    bool
		wantErr   bool
	}{
		{raw: "# 注释", wantOK: false},
		{raw: "   ", wantOK: false},
		{raw: "KEY=value", wantKey: "KEY", wantValue: "value", wantOK: true},
		{raw: "export KEY=value", wantKey: "KEY", wantValue: "value", wantOK: true},
		{raw: `KEY="a b c"`, wantKey: "KEY", wantValue: "a b c", wantOK: true},
		{raw: "KEY='x#y'", wantKey: "KEY", wantValue: "x#y", wantOK: true},
		{raw: "KEY=", wantKey: "KEY", wantValue: "", wantOK: true},
		{raw: "NOEQUALS", wantErr: true},
		{raw: "=value", wantErr: true},
	}

	for _, tc := range cases {
		key, value, ok, err := parseEnvLine(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseEnvLine(%q) 期望报错", tc.raw)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseEnvLine(%q) 意外报错: %v", tc.raw, err)
			continue
		}
		if ok != tc.wantOK {
			t.Errorf("parseEnvLine(%q) ok = %v，期望 %v", tc.raw, ok, tc.wantOK)
			continue
		}
		if ok && (key != tc.wantKey || value != tc.wantValue) {
			t.Errorf("parseEnvLine(%q) = (%q, %q)，期望 (%q, %q)", tc.raw, key, value, tc.wantKey, tc.wantValue)
		}
	}
}

func TestLoadEnvFileDoesNotOverrideExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := strings.Join([]string{
		"# 注释行",
		"",
		"NEWX_TEST_NEW_KEY=from-file",
		`NEWX_TEST_KEEP_KEY="from-file-but-should-not-win"`,
		"export NEWX_TEST_EXPORTED=ok",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入临时 env 文件失败: %v", err)
	}

	t.Setenv("NEWX_TEST_KEEP_KEY", "from-process")
	// 清理可能被注入的键，避免污染后续测试。
	t.Cleanup(func() {
		os.Unsetenv("NEWX_TEST_NEW_KEY")
		os.Unsetenv("NEWX_TEST_EXPORTED")
	})

	applied, skipped, err := LoadEnvFile(path)
	if err != nil {
		t.Fatalf("加载 env 文件失败: %v", err)
	}
	if applied != 2 || skipped != 1 {
		t.Errorf("applied = %d, skipped = %d，期望 2 与 1", applied, skipped)
	}
	if got := os.Getenv("NEWX_TEST_NEW_KEY"); got != "from-file" {
		t.Errorf("新键未注入，实际 %q", got)
	}
	if got := os.Getenv("NEWX_TEST_KEEP_KEY"); got != "from-process" {
		t.Errorf("已存在的进程变量被覆盖，实际 %q", got)
	}
	if got := os.Getenv("NEWX_TEST_EXPORTED"); got != "ok" {
		t.Errorf("export 前缀行未注入，实际 %q", got)
	}
}

func TestLoadEnvFileMissingFile(t *testing.T) {
	if _, _, err := LoadEnvFile(filepath.Join(t.TempDir(), "not-exist.env")); err == nil {
		t.Fatal("文件不存在时应报错")
	}
}

func TestDurationAndIntHelpersRespectFallback(t *testing.T) {
	var errs []error
	reader := envReader{lookup: newLookup(map[string]string{})}
	if got := reader.duration("TIMEOUT_READ", 5*time.Second, &errs); got != 5*time.Second {
		t.Errorf("缺失时应用默认值，实际 %s", got)
	}
	if len(errs) != 0 {
		t.Errorf("缺失时不应产生错误，实际 %v", errs)
	}
}
