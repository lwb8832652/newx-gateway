// Package config 负责把环境变量解析为强类型配置，并在启动期完成校验。
//
// 约定：
//   - 只从环境变量读取，不解析额外配置文件（可选 env 文件由 envfile.go 先行注入环境变量）。
//   - 所有默认值集中在本文件，禁止在业务代码里散落默认值。
//   - 校验失败立即返回错误，由入口打印明确原因并以非零码退出，不做静默降级。
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 运行环境取值。
const (
	EnvDev        = "dev"
	EnvProduction = "production"
	EnvTest       = "test"
)

// 日志相关取值。
const (
	LogFormatJSON = "json"
	LogFormatText = "text"
)

// 默认值。修改默认值必须同步更新 .env.example 与 README。
const (
	DefaultHTTPAddr             = ":8080"
	DefaultEnv                  = EnvDev
	DefaultMCPEnabled           = true
	DefaultOpenAPIEnabled       = false
	DefaultMaxBodyBytes         = 1 << 20
	DefaultMaxBodyBytesUpload   = 2 << 30
	DefaultLogLevel             = "info"
	DefaultLogFormat            = LogFormatJSON
	DefaultExitWaitTime         = 10 * time.Second
	DefaultHuobaoBaseURL        = "http://127.0.0.1:5679"
	DefaultAuthIssuerBaseURL    = "http://127.0.0.1:9099"
	DefaultTimeoutRead          = 15 * time.Second
	DefaultTimeoutWrite         = 60 * time.Second
	DefaultTimeoutGenerate      = 900 * time.Second
	DefaultTimeoutUpload        = 1800 * time.Second
	DefaultTimeoutStatic        = 300 * time.Second
	DefaultTimeoutMCP           = 60 * time.Second
	DefaultRateWeb              = 120
	DefaultRateWebGenerate      = 10
	DefaultRateAdmin            = 300
	DefaultRateMCP              = 60
	DefaultMaxConcurrencyGen    = 32
	DefaultReadyzProbeDownstrm  = false
	DefaultTrustProxyHeaders    = false
	DefaultAuthStubEnabled      = false
	appCodePattern              = `^[a-z][a-z0-9-]{1,31}$`
)

var appCodeRe = regexp.MustCompile(appCodePattern)

// AppConfig 是一条应用注册记录：应用编码与下游地址。
// 地址可以为空，表示该应用已注册但下游尚未接入。
type AppConfig struct {
	Code    string
	BaseURL string
}

// Timeouts 是按路由类别划分的超时预算。
// 本阶段仅解析与校验，尚未参与转发逻辑（反代接入后启用）。
type Timeouts struct {
	Read     time.Duration
	Write    time.Duration
	Generate time.Duration
	Upload   time.Duration
	Static   time.Duration
	MCP      time.Duration
}

// RateLimits 是限流分桶阈值。
// 本阶段仅解析与校验，尚未生效（限流中间件接入后启用）。
type RateLimits struct {
	Web                    int
	WebGenerate            int
	Admin                  int
	MCP                    int
	MaxConcurrencyGenerate int
}

// Config 是入口运行期配置。
type Config struct {
	HTTPAddr     string
	Env          string
	Version      string
	ExitWaitTime time.Duration

	Apps           []AppConfig
	MCPEnabled     bool
	OpenAPIEnabled bool

	AuthIssuerBaseURL string
	AuthStubEnabled   bool

	MaxBodyBytes       int64
	MaxBodyBytesUpload int64

	Timeouts Timeouts
	Rates    RateLimits

	LogLevel              string
	LogFormat             string
	ReadyzProbeDownstream bool
	TrustProxyHeaders     bool
}

// Load 从进程环境变量加载配置并校验。
func Load() (*Config, error) {
	return LoadFrom(os.LookupEnv)
}

// LoadFrom 从给定的读取函数加载配置并校验，供测试使用。
func LoadFrom(lookup func(string) (string, bool)) (*Config, error) {
	cfg, err := parse(lookup)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// IsProduction 报告当前是否为生产环境。生产环境会触发额外红线校验。
func (c *Config) IsProduction() bool { return c.Env == EnvProduction }

// IsJSONLog 报告日志是否输出为 JSON。
func (c *Config) IsJSONLog() bool { return c.LogFormat == LogFormatJSON }

// AppByCode 返回指定编码的应用注册记录。
func (c *Config) AppByCode(code string) (AppConfig, bool) {
	for _, app := range c.Apps {
		if app.Code == code {
			return app, true
		}
	}
	return AppConfig{}, false
}

func parse(lookup func(string) (string, bool)) (*Config, error) {
	reader := envReader{lookup: lookup}
	var errs []error

	cfg := &Config{
		HTTPAddr:     reader.str("ENTRY_HTTP_ADDR", DefaultHTTPAddr),
		Env:          strings.ToLower(reader.str("ENTRY_ENV", DefaultEnv)),
		Version:      reader.str("ENTRY_VERSION", "dev"),
		ExitWaitTime: reader.duration("ENTRY_EXIT_WAIT_TIME", DefaultExitWaitTime, &errs),

		MCPEnabled:     reader.boolean("MCP_ENABLED", DefaultMCPEnabled, &errs),
		OpenAPIEnabled: reader.boolean("OPENAPI_ENABLED", DefaultOpenAPIEnabled, &errs),

		AuthIssuerBaseURL: reader.str("AUTH_ISSUER_BASE_URL", DefaultAuthIssuerBaseURL),
		AuthStubEnabled:   reader.boolean("AUTH_STUB_ENABLED", DefaultAuthStubEnabled, &errs),

		MaxBodyBytes:       reader.int64("MAX_BODY_BYTES", DefaultMaxBodyBytes, &errs),
		MaxBodyBytesUpload: reader.int64("MAX_BODY_BYTES_UPLOAD", DefaultMaxBodyBytesUpload, &errs),

		Timeouts: Timeouts{
			Read:     reader.duration("TIMEOUT_READ", DefaultTimeoutRead, &errs),
			Write:    reader.duration("TIMEOUT_WRITE", DefaultTimeoutWrite, &errs),
			Generate: reader.duration("TIMEOUT_GENERATE", DefaultTimeoutGenerate, &errs),
			Upload:   reader.duration("TIMEOUT_UPLOAD", DefaultTimeoutUpload, &errs),
			Static:   reader.duration("TIMEOUT_STATIC", DefaultTimeoutStatic, &errs),
			MCP:      reader.duration("TIMEOUT_MCP", DefaultTimeoutMCP, &errs),
		},
		Rates: RateLimits{
			Web:                    reader.int("RATE_WEB", DefaultRateWeb, &errs),
			WebGenerate:            reader.int("RATE_WEB_GENERATE", DefaultRateWebGenerate, &errs),
			Admin:                  reader.int("RATE_ADMIN", DefaultRateAdmin, &errs),
			MCP:                    reader.int("RATE_MCP", DefaultRateMCP, &errs),
			MaxConcurrencyGenerate: reader.int("MAX_CONCURRENCY_GENERATE", DefaultMaxConcurrencyGen, &errs),
		},

		LogLevel:              strings.ToLower(reader.str("LOG_LEVEL", DefaultLogLevel)),
		LogFormat:             strings.ToLower(reader.str("LOG_FORMAT", DefaultLogFormat)),
		ReadyzProbeDownstream: reader.boolean("READYZ_PROBE_DOWNSTREAM", DefaultReadyzProbeDownstrm, &errs),
		TrustProxyHeaders:     reader.boolean("TRUST_PROXY_HEADERS", DefaultTrustProxyHeaders, &errs),
	}

	apps, appErr := parseApps(reader)
	if appErr != nil {
		errs = append(errs, appErr)
	}
	cfg.Apps = apps

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cfg, nil
}

// parseApps 解析 APPS。缺省时按 HUOBAO_BASE_URL 生成单条 huobao 注册记录。
func parseApps(reader envReader) ([]AppConfig, error) {
	raw := reader.str("APPS", "")
	huobaoBaseURL := reader.str("HUOBAO_BASE_URL", DefaultHuobaoBaseURL)
	if strings.TrimSpace(raw) == "" {
		return []AppConfig{{Code: "huobao", BaseURL: huobaoBaseURL}}, nil
	}

	entries := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' })
	apps := make([]AppConfig, 0, len(entries))
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		code, baseURL, hasURL := strings.Cut(entry, "=")
		code = strings.TrimSpace(code)
		if code == "" {
			return nil, fmt.Errorf("APPS 条目 %q 缺少应用编码", entry)
		}
		app := AppConfig{Code: code}
		if hasURL {
			app.BaseURL = strings.TrimSpace(baseURL)
		} else if code == "huobao" {
			app.BaseURL = huobaoBaseURL
		}
		apps = append(apps, app)
	}
	if len(apps) == 0 {
		return nil, errors.New("APPS 未解析出任何有效应用")
	}
	return apps, nil
}

// Validate 执行启动期校验。任何失败都必须让进程无法启动。
func (c *Config) Validate() error {
	var errs []error

	if err := validateListenAddr(c.HTTPAddr); err != nil {
		errs = append(errs, fmt.Errorf("ENTRY_HTTP_ADDR 非法: %w", err))
	}

	switch c.Env {
	case EnvDev, EnvProduction, EnvTest:
	default:
		errs = append(errs, fmt.Errorf("ENTRY_ENV 取值非法: %q（可选 %s/%s/%s）", c.Env, EnvDev, EnvProduction, EnvTest))
	}

	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("LOG_LEVEL 取值非法: %q（可选 debug/info/warn/error）", c.LogLevel))
	}

	switch c.LogFormat {
	case LogFormatJSON, LogFormatText:
	default:
		errs = append(errs, fmt.Errorf("LOG_FORMAT 取值非法: %q（可选 %s/%s）", c.LogFormat, LogFormatJSON, LogFormatText))
	}

	if c.ExitWaitTime <= 0 {
		errs = append(errs, fmt.Errorf("ENTRY_EXIT_WAIT_TIME 必须为正数: %s", c.ExitWaitTime))
	}

	if len(c.Apps) == 0 {
		errs = append(errs, errors.New("APPS 至少需要一个应用注册记录"))
	}
	seen := make(map[string]struct{}, len(c.Apps))
	for _, app := range c.Apps {
		if !IsValidAppCode(app.Code) {
			errs = append(errs, fmt.Errorf("APPS 应用编码非法: %q（需匹配 %s）", app.Code, appCodePattern))
		}
		if _, dup := seen[app.Code]; dup {
			errs = append(errs, fmt.Errorf("APPS 应用编码重复: %q", app.Code))
		}
		seen[app.Code] = struct{}{}
		if app.BaseURL != "" {
			if err := validateDownstreamURL(app.BaseURL); err != nil {
				errs = append(errs, fmt.Errorf("APPS 应用 %q 的下游地址非法: %w", app.Code, err))
			}
		}
	}

	if c.AuthIssuerBaseURL != "" {
		if err := validateDownstreamURL(c.AuthIssuerBaseURL); err != nil {
			errs = append(errs, fmt.Errorf("AUTH_ISSUER_BASE_URL 非法: %w", err))
		}
	}

	if c.MaxBodyBytes <= 0 {
		errs = append(errs, fmt.Errorf("MAX_BODY_BYTES 必须为正数: %d", c.MaxBodyBytes))
	}
	if c.MaxBodyBytesUpload <= 0 {
		errs = append(errs, fmt.Errorf("MAX_BODY_BYTES_UPLOAD 必须为正数: %d", c.MaxBodyBytesUpload))
	}

	// 生产红线：内置签发桩绝不允许进入生产环境。
	if c.IsProduction() && c.AuthStubEnabled {
		errs = append(errs, errors.New("生产环境禁止开启 AUTH_STUB_ENABLED（签发桩仅限 dev/CI）"))
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// IsValidAppCode 校验应用编码格式。注册表与应用配置共用同一规则，避免两处漂移。
func IsValidAppCode(code string) bool { return appCodeRe.MatchString(code) }

func validateListenAddr(addr string) error {
	if strings.TrimSpace(addr) == "" {
		return errors.New("监听地址为空")
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("需为 host:port 形式: %w", err)
	}
	value, err := strconv.Atoi(port)
	if err != nil || value < 1 || value > 65535 {
		return fmt.Errorf("端口取值非法: %q", port)
	}
	return nil
}

func validateDownstreamURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("仅支持 http/https，实际 %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return errors.New("缺少主机名")
	}
	return nil
}

type envReader struct {
	lookup func(string) (string, bool)
}

func (r envReader) str(key, fallback string) string {
	if value, ok := r.lookup(key); ok {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return fallback
}

func (r envReader) int(key string, fallback int, errs *[]error) int {
	raw, ok := r.lookup(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s 需为整数: %q", key, raw))
		return fallback
	}
	return value
}

func (r envReader) int64(key string, fallback int64, errs *[]error) int64 {
	raw, ok := r.lookup(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s 需为整数: %q", key, raw))
		return fallback
	}
	return value
}

func (r envReader) duration(key string, fallback time.Duration, errs *[]error) time.Duration {
	raw, ok := r.lookup(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	value, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s 需为时间长度（如 15s、5m）: %q", key, raw))
		return fallback
	}
	return value
}

func (r envReader) boolean(key string, fallback bool, errs *[]error) bool {
	raw, ok := r.lookup(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	value, err := parseBool(strings.TrimSpace(raw))
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s 需为布尔值: %q", key, raw))
		return fallback
	}
	return value
}

func parseBool(raw string) (bool, error) {
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on", "y":
		return true, nil
	case "0", "false", "no", "off", "n":
		return false, nil
	default:
		return false, fmt.Errorf("无法识别的布尔值 %q", raw)
	}
}
