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
	DefaultHTTPAddr       = ":8080"
	DefaultEnv            = EnvDev
	DefaultMCPEnabled     = true
	DefaultOpenAPIEnabled = false
	// DefaultMaxBodyBytes 与 yudao-gateway 的 max-in-memory-size 对齐：
	// 后台的批量导入、代码生成等 JSON 请求体可能超过 1MB，
	// 收紧到 1MB 会把正常业务请求挡在入口（返回 40002）。
	DefaultMaxBodyBytes       = 10 << 20
	DefaultMaxBodyBytesUpload = 2 << 30
	DefaultLogLevel           = "info"
	DefaultLogFormat          = LogFormatJSON
	DefaultExitWaitTime       = 10 * time.Second
	DefaultHuobaoBaseURL      = "http://127.0.0.1:5679"
	DefaultAuthIssuerBaseURL  = "http://127.0.0.1:9099"
	// DefaultTimeoutRead 比最初的 15s 放宽：yudao 网关不设响应超时，
	// 后台导出、文件下载、长轮询都走 GET，15s 会在业务返回前先 504。
	DefaultTimeoutRead     = 60 * time.Second
	DefaultTimeoutWrite    = 60 * time.Second
	DefaultTimeoutGenerate = 900 * time.Second
	DefaultTimeoutUpload   = 1800 * time.Second
	DefaultTimeoutStatic   = 300 * time.Second
	DefaultTimeoutMCP      = 60 * time.Second
	// DefaultTimeoutStream 是流式响应（SSE/MCP 长连接）的整体预算，
	// 0 表示不设上限：AI 对话与 MCP 会话可能持续数小时。
	// 连接与响应头阶段仍受读/写预算约束，不会因为上游不响应而永久挂住。
	DefaultTimeoutStream = time.Duration(0)
	// DefaultWSHandshakeTimeout 是 WebSocket 握手（建连 + 101 响应）的超时。
	// 上游只建 TCP 连接却不回 101 时，必须有兜底，否则连接会一直悬挂。
	DefaultWSHandshakeTimeout  = 10 * time.Second
	DefaultRateWeb             = 120
	DefaultRateWebGenerate     = 10
	DefaultRateAdmin           = 300
	DefaultRateMCP             = 60
	DefaultMaxConcurrencyGen   = 32
	DefaultReadyzProbeDownstrm = false
	DefaultTrustProxyHeaders   = false
	DefaultAuthStubEnabled     = false
	DefaultDiscoveryRefresh    = 5 * time.Second
	DefaultBreakerEnabled      = true
	DefaultBreakerThreshold    = 5
	DefaultBreakerCooldown     = 10 * time.Second
	// DefaultAuthCacheTTL 与 yudao 网关的 Guava 缓存时长保持一致。
	DefaultAuthCacheTTL = time.Minute
	DefaultAuthTimeout  = 3 * time.Second
	// DefaultAuthCheckPath 是 yudao-cloud 的令牌校验接口
	// （OAuth2TokenCommonApi.URL_CHECK = /rpc-api/system/oauth2/token/check）。
	// 单体版 ruoyi-vue-pro 的 /system-api/oauth2/check-token 需显式覆盖。
	DefaultAuthCheckPath = "/rpc-api/system/oauth2/token/check"
	DefaultAuthEnabled   = false
	// DefaultGeneratePaths 与 DefaultUploadPaths 对应设计文档 §6 的预算分类。
	// /export、/download 覆盖 yudao 后台的导出与文件下载：它们是 GET，
	// 但耗时远超普通读请求，必须走长预算，否则会被 504 截断。
	DefaultGeneratePaths = "/agent,/tasks,/merge,/previs,/task-queue,/export,/download"
	DefaultUploadPaths   = "/upload"
	// DefaultStreamPaths 覆盖 AI SSE、MCP 的旧/新路径。
	// 用不带前导斜杠的 stream，是为了同时匹配 /chat/stream 与 /chat/message/send-stream
	// （后者在 stream 前是连字符，"/stream" 匹配不到）。
	DefaultStreamPaths = "/sse,stream,/events,/mcp"
	appCodePattern     = `^[a-z][a-z0-9-]{1,31}$`
)

var appCodeRe = regexp.MustCompile(appCodePattern)

// LegacyRoute 是 yudao 旧路径兼容映射：入口前缀 → 应用编码。
//
// yudao-gateway 用 /admin-api/{module}/**、/app-api/{module}/** 这类
// 「路径即服务」的规则；本工程默认使用 /admin/{app_code}/v1/** 的
// 「路径即应用」规则。存量前端与第三方系统仍调用旧路径，因此提供显式映射：
// 命中后保留原始路径原样转发，不做任何改写，行为与 yudao 的 Path 断言一致。
type LegacyRoute struct {
	// Prefix 是入口路径前缀，必须以 / 开头，例如 /admin-api/system。
	Prefix string
	// AppCode 是注册表中的应用编码，例如 yudao-system。
	AppCode string
}

// AppConfig 是一条应用注册记录：应用编码与下游来源。
// 地址可以为空（表示已注册但下游尚未接入），但静态地址与注册中心服务至少要有一个。
type AppConfig struct {
	Code    string
	BaseURL string
	// Service 是注册中心服务名，来自 APPS 中的 nacos:// 形式；为空表示使用静态地址。
	Service string
	// UpstreamPath 是上游路径前缀：入口剥掉自身前缀后，再拼上它转发给下游。
	// 例如入口 /admin/yudao-system/v1/user/page → 上游 /admin-api/system/user/page。
	UpstreamPath string
}

// Discovery 是服务发现配置。
type Discovery struct {
	// Enabled 为 true 时使用 Nacos 开放 API 解析 nacos:// 形式的服务。
	Enabled bool
	// Addr 是 Nacos 地址（host:port）。
	Addr string
	// Namespace 是命名空间 ID，为空使用 public。
	Namespace string
	// Group 是分组名，为空使用 DEFAULT_GROUP。
	Group string
	// Refresh 是后台刷新周期，同时作为本地缓存的 TTL。
	Refresh time.Duration
}

// Breaker 是失败实例熔断配置。
//
// 作用：某个实例连续失败达到阈值后，在冷却期内不再被选中，
// 避免坏实例持续拖慢请求（静态地址场景尤其重要，因为没有注册中心的健康检查）。
type Breaker struct {
	Enabled          bool
	FailureThreshold int
	Cooldown         time.Duration
}

// TimeoutRules 用路径片段把请求分类到不同的超时预算。
//
// 设计文档 §6 的四级预算（读/写/生成/上传）需要知道请求类型，
// 而入口不做业务路由登记，因此用可配置的路径片段匹配来近似分类。
type TimeoutRules struct {
	// GeneratePaths 命中则按生成类预算（默认 15min）。
	GeneratePaths []string
	// UploadPaths 命中则按上传类预算（默认 30min）。
	UploadPaths []string
	// StreamPaths 命中则按流式预算（默认不设上限），优先于生成/读写预算。
	StreamPaths []string
}

// Auth 是认证配置（沿用 yudao-cloud 网关的认证方式）。
type Auth struct {
	// Enabled 是否启用身份解析。关闭时中间件直接放行，不注入任何身份头。
	Enabled bool
	// AppCode 是鉴权方在注册表中的应用编码；为空表示使用静态地址。
	AppCode string
	// BaseURL 是鉴权方静态地址，AppCode 为空时使用。
	BaseURL string
	// CheckPath 是令牌校验接口路径，默认 /system-api/oauth2/check-token。
	CheckPath string
	// CacheTTL 是登录用户缓存时长，默认 1 分钟（与 yudao 一致）。
	CacheTTL time.Duration
	// Timeout 是单次校验的超时。
	Timeout time.Duration
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
	// Stream 是 SSE/MCP 长连接的整体预算，0 表示不设上限。
	Stream time.Duration
	// WSHandshake 是 WebSocket 握手阶段的超时。
	WSHandshake time.Duration
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
	LegacyRoutes   []LegacyRoute
	MCPEnabled     bool
	OpenAPIEnabled bool

	Discovery Discovery

	Breaker      Breaker
	TimeoutRules TimeoutRules

	AuthIssuerBaseURL string
	AuthStubEnabled   bool
	// Auth 是 yudao 方式认证的开关与参数。
	Auth Auth

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
		Auth: Auth{
			Enabled:   reader.boolean("AUTH_ENABLED", DefaultAuthEnabled, &errs),
			AppCode:   reader.str("AUTH_APP_CODE", ""),
			BaseURL:   reader.str("AUTH_BASE_URL", ""),
			CheckPath: reader.str("AUTH_CHECK_PATH", DefaultAuthCheckPath),
			CacheTTL:  reader.duration("AUTH_CACHE_TTL", DefaultAuthCacheTTL, &errs),
			Timeout:   reader.duration("AUTH_TIMEOUT", DefaultAuthTimeout, &errs),
		},

		MaxBodyBytes:       reader.int64("MAX_BODY_BYTES", DefaultMaxBodyBytes, &errs),
		MaxBodyBytesUpload: reader.int64("MAX_BODY_BYTES_UPLOAD", DefaultMaxBodyBytesUpload, &errs),

		Timeouts: Timeouts{
			Read:        reader.duration("TIMEOUT_READ", DefaultTimeoutRead, &errs),
			Write:       reader.duration("TIMEOUT_WRITE", DefaultTimeoutWrite, &errs),
			Generate:    reader.duration("TIMEOUT_GENERATE", DefaultTimeoutGenerate, &errs),
			Upload:      reader.duration("TIMEOUT_UPLOAD", DefaultTimeoutUpload, &errs),
			Static:      reader.duration("TIMEOUT_STATIC", DefaultTimeoutStatic, &errs),
			MCP:         reader.duration("TIMEOUT_MCP", DefaultTimeoutMCP, &errs),
			Stream:      reader.duration("TIMEOUT_STREAM", DefaultTimeoutStream, &errs),
			WSHandshake: reader.duration("TIMEOUT_WS_HANDSHAKE", DefaultWSHandshakeTimeout, &errs),
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

	legacyRoutes, legacyErr := parseLegacyRoutes(reader)
	if legacyErr != nil {
		errs = append(errs, legacyErr)
	}
	cfg.LegacyRoutes = legacyRoutes

	discovery, discoveryErr := parseDiscovery(reader)
	if discoveryErr != nil {
		errs = append(errs, discoveryErr)
	}
	cfg.Discovery = discovery

	cfg.Breaker = Breaker{
		Enabled:          reader.boolean("BREAKER_ENABLED", DefaultBreakerEnabled, &errs),
		FailureThreshold: reader.int("BREAKER_FAIL_THRESHOLD", DefaultBreakerThreshold, &errs),
		Cooldown:         reader.duration("BREAKER_COOLDOWN", DefaultBreakerCooldown, &errs),
	}

	generatePaths, uploadPaths, streamPaths := parsePathRules(reader)
	cfg.TimeoutRules = TimeoutRules{
		GeneratePaths: generatePaths,
		UploadPaths:   uploadPaths,
		StreamPaths:   streamPaths,
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cfg, nil
}

// parseDiscovery 解析服务发现配置。语法：DISCOVERY=nacos://host:port。
func parseDiscovery(reader envReader) (Discovery, error) {
	var discovery Discovery

	raw := reader.str("DISCOVERY", "")
	if raw != "" {
		addr, ok := strings.CutPrefix(raw, "nacos://")
		if !ok || strings.TrimSpace(addr) == "" {
			return discovery, fmt.Errorf("DISCOVERY 仅支持 nacos://host:port 形式: %q", raw)
		}
		discovery.Enabled = true
		discovery.Addr = strings.TrimSpace(addr)
	}

	discovery.Namespace = reader.str("DISCOVERY_NAMESPACE", "")
	discovery.Group = reader.str("DISCOVERY_GROUP", "")

	var errs []error
	discovery.Refresh = reader.duration("DISCOVERY_REFRESH", DefaultDiscoveryRefresh, &errs)
	if len(errs) > 0 {
		return discovery, errors.Join(errs...)
	}
	return discovery, nil
}

// parseApps 解析 APPS。缺省时按 HUOBAO_BASE_URL 生成单条 huobao 注册记录。
//
// 条目语法（三部分均可选）：
//
//	code                    仅注册编码，地址待接入
//	code=url                静态地址
//	code=nacos://service    注册中心服务名
//	任意形式后接 |/upstream   上游路径前缀，用于路径改写
//
// 例：APPS=huobao=http://127.0.0.1:5679|/api/v1,yudao-system=nacos://yudao-system|/admin-api/system
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

		code, remainder, hasAddress := strings.Cut(entry, "=")
		code = strings.TrimSpace(code)
		if code == "" {
			return nil, fmt.Errorf("APPS 条目 %q 缺少应用编码", entry)
		}

		app := AppConfig{Code: code}
		if hasAddress {
			address, upstreamPath, hasPath := strings.Cut(remainder, "|")
			address = strings.TrimSpace(address)
			if service, ok := strings.CutPrefix(address, "nacos://"); ok {
				app.Service = strings.TrimSpace(service)
			} else {
				app.BaseURL = address
			}
			if hasPath {
				app.UpstreamPath = strings.TrimSpace(upstreamPath)
			}
		} else if code == "huobao" {
			// 兼容旧写法：只写 huobao 时回落到 HUOBAO_BASE_URL。
			app.BaseURL = huobaoBaseURL
		}
		apps = append(apps, app)
	}
	if len(apps) == 0 {
		return nil, errors.New("APPS 未解析出任何有效应用")
	}
	return apps, nil
}

// parseLegacyRoutes 解析 yudao 旧路径兼容映射。
//
// 语法：LEGACY_ROUTES=/admin-api/system=yudao-system,/app-api/system=yudao-system
// 前缀必须以 / 开头；分隔符与 APPS 一致（逗号或分号）。
// 留空表示不开启旧路径兼容，入口只接受 /{web|admin|mcp|open-api}/{app_code} 形式。
func parseLegacyRoutes(reader envReader) ([]LegacyRoute, error) {
	raw := reader.str("LEGACY_ROUTES", "")
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	entries := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' })
	routes := make([]LegacyRoute, 0, len(entries))
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		prefix, code, ok := strings.Cut(entry, "=")
		prefix = strings.TrimSpace(prefix)
		code = strings.TrimSpace(code)
		if !ok || prefix == "" || code == "" {
			return nil, fmt.Errorf("LEGACY_ROUTES 条目 %q 语法应为 前缀=应用编码", entry)
		}
		routes = append(routes, LegacyRoute{
			Prefix:  strings.TrimSuffix(prefix, "/"),
			AppCode: code,
		})
	}
	if len(routes) == 0 {
		return nil, errors.New("LEGACY_ROUTES 未解析出任何有效条目")
	}
	return routes, nil
}

// parsePathRules 解析超时分类的路径片段规则。
// 空串表示使用默认规则；条目需以 / 开头，否则视为配置错误。
func parsePathRules(reader envReader) (generate, upload, stream []string) {
	generate = splitPathList(reader.str("TIMEOUT_GENERATE_PATHS", DefaultGeneratePaths))
	upload = splitPathList(reader.str("TIMEOUT_UPLOAD_PATHS", DefaultUploadPaths))
	stream = splitPathList(reader.str("STREAM_PATHS", DefaultStreamPaths))
	return generate, upload, stream
}

func splitPathList(raw string) []string {
	items := strings.Split(raw, ",")
	result := make([]string, 0, len(items))
	for _, item := range items {
		trimmed := strings.TrimSpace(item)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
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

		// 每个应用必须有可用的下游来源，否则请求会在运行期才失败。
		if app.BaseURL == "" && app.Service == "" {
			errs = append(errs, fmt.Errorf("APPS 应用 %q 既无静态地址也无注册中心服务", app.Code))
		}
		if app.BaseURL != "" {
			if err := validateDownstreamURL(app.BaseURL); err != nil {
				errs = append(errs, fmt.Errorf("APPS 应用 %q 的下游地址非法: %w", app.Code, err))
			}
		}
		if app.Service != "" && !IsValidAppCode(app.Service) {
			errs = append(errs, fmt.Errorf("APPS 应用 %q 的注册中心服务名非法: %q", app.Code, app.Service))
		}
		if app.UpstreamPath != "" && !strings.HasPrefix(app.UpstreamPath, "/") {
			errs = append(errs, fmt.Errorf("APPS 应用 %q 的上游路径需以 / 开头: %q", app.Code, app.UpstreamPath))
		}
	}

	// 旧路径兼容映射：前缀必须可路由，指向的应用必须真实存在，
	// 否则请求会静默打到错误服务或反复 50201。
	legacySeen := make(map[string]struct{}, len(c.LegacyRoutes))
	for _, route := range c.LegacyRoutes {
		if !strings.HasPrefix(route.Prefix, "/") || route.Prefix == "/" {
			errs = append(errs, fmt.Errorf("LEGACY_ROUTES 前缀 %q 必须以 / 开头且不能是根路径", route.Prefix))
		}
		if strings.ContainsAny(route.Prefix, " ?#") || strings.Contains(route.Prefix, "//") {
			errs = append(errs, fmt.Errorf("LEGACY_ROUTES 前缀 %q 含非法字符", route.Prefix))
		}
		if _, dup := legacySeen[route.Prefix]; dup {
			errs = append(errs, fmt.Errorf("LEGACY_ROUTES 前缀重复: %q", route.Prefix))
		}
		legacySeen[route.Prefix] = struct{}{}
		if _, ok := c.AppByCode(route.AppCode); !ok {
			errs = append(errs, fmt.Errorf("LEGACY_ROUTES 前缀 %q 指向的应用 %q 不在 APPS 注册表中", route.Prefix, route.AppCode))
		}
	}

	if c.Discovery.Enabled {
		if err := validateListenAddr(c.Discovery.Addr); err != nil {
			errs = append(errs, fmt.Errorf("DISCOVERY 地址非法: %w", err))
		}
		if c.Discovery.Refresh <= 0 {
			errs = append(errs, fmt.Errorf("DISCOVERY_REFRESH 必须为正数: %s", c.Discovery.Refresh))
		}
	} else {
		// 反向校验：用了 nacos:// 服务却没开服务发现，启动时必须指出，
		// 否则要等第一个请求进来才以 50201 失败。
		for _, app := range c.Apps {
			if app.Service != "" {
				errs = append(errs, fmt.Errorf("APPS 应用 %q 使用 nacos:// 服务，但 DISCOVERY 未配置", app.Code))
			}
		}
	}

	if c.Breaker.Enabled {
		if c.Breaker.FailureThreshold <= 0 {
			errs = append(errs, fmt.Errorf("BREAKER_FAIL_THRESHOLD 必须为正数: %d", c.Breaker.FailureThreshold))
		}
		if c.Breaker.Cooldown <= 0 {
			errs = append(errs, fmt.Errorf("BREAKER_COOLDOWN 必须为正数: %s", c.Breaker.Cooldown))
		}
	}
	if err := validatePathRules("TIMEOUT_GENERATE_PATHS", c.TimeoutRules.GeneratePaths); err != nil {
		errs = append(errs, err)
	}
	if err := validatePathRules("TIMEOUT_UPLOAD_PATHS", c.TimeoutRules.UploadPaths); err != nil {
		errs = append(errs, err)
	}
	// STREAM_PATHS 允许为空：此时仍可通过 Accept: text/event-stream 识别流式请求。
	if err := validateOptionalPathRules("STREAM_PATHS", c.TimeoutRules.StreamPaths); err != nil {
		errs = append(errs, err)
	}
	// TIMEOUT_STREAM=0 是合法值（不设上限）；负值会让流式请求立即超时，必须拒绝。
	if c.Timeouts.Stream < 0 {
		errs = append(errs, fmt.Errorf("TIMEOUT_STREAM 不能为负数: %s", c.Timeouts.Stream))
	}
	if c.Timeouts.WSHandshake <= 0 {
		errs = append(errs, fmt.Errorf("TIMEOUT_WS_HANDSHAKE 必须为正数: %s", c.Timeouts.WSHandshake))
	}

	if c.AuthIssuerBaseURL != "" {
		if err := validateDownstreamURL(c.AuthIssuerBaseURL); err != nil {
			errs = append(errs, fmt.Errorf("AUTH_ISSUER_BASE_URL 非法: %w", err))
		}
	}

	// 认证启用时必须给出可用的鉴权方来源，且两个来源只能选一个，
	// 否则「到底连哪个」要到运行期才能发现，属于配置歧义。
	if c.Auth.Enabled {
		if c.Auth.AppCode == "" && c.Auth.BaseURL == "" {
			errs = append(errs, errors.New("AUTH_ENABLED=true 时需要配置 AUTH_APP_CODE 或 AUTH_BASE_URL"))
		}
		if c.Auth.AppCode != "" && c.Auth.BaseURL != "" {
			errs = append(errs, errors.New("AUTH_APP_CODE 与 AUTH_BASE_URL 只能配置一个"))
		}
		if c.Auth.AppCode != "" {
			if _, ok := c.AppByCode(c.Auth.AppCode); !ok {
				errs = append(errs, fmt.Errorf("AUTH_APP_CODE %q 不在 APPS 注册表中", c.Auth.AppCode))
			}
		}
		if c.Auth.BaseURL != "" {
			if err := validateDownstreamURL(c.Auth.BaseURL); err != nil {
				errs = append(errs, fmt.Errorf("AUTH_BASE_URL 非法: %w", err))
			}
		}
		// CheckPath 为空表示使用默认值，只有显式配了才校验格式。
		if c.Auth.CheckPath != "" && !strings.HasPrefix(c.Auth.CheckPath, "/") {
			errs = append(errs, fmt.Errorf("AUTH_CHECK_PATH 需以 / 开头: %q", c.Auth.CheckPath))
		}
		if c.Auth.CacheTTL <= 0 {
			errs = append(errs, fmt.Errorf("AUTH_CACHE_TTL 必须为正数: %s", c.Auth.CacheTTL))
		}
		if c.Auth.Timeout <= 0 {
			errs = append(errs, fmt.Errorf("AUTH_TIMEOUT 必须为正数: %s", c.Auth.Timeout))
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

// validatePathRules 校验超时分类路径片段：非空且以 / 开头。
// validateOptionalPathRules 校验可留空的路径片段列表。
// 流式片段按「子串」匹配，允许不带前导斜杠（例如 stream 同时覆盖
// /chat/stream 与 /chat/message/send-stream）；留空表示只依赖 Accept 头识别。
func validateOptionalPathRules(name string, paths []string) error {
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			return fmt.Errorf("%s 含空白片段", name)
		}
	}
	return nil
}

func validatePathRules(name string, paths []string) error {
	if len(paths) == 0 {
		return fmt.Errorf("%s 至少需要一个路径片段", name)
	}
	for _, path := range paths {
		if !strings.HasPrefix(path, "/") {
			return fmt.Errorf("%s 片段 %q 需以 / 开头", name, path)
		}
	}
	return nil
}

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
