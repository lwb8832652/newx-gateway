// Package app 是入口的装配层：把配置、注册表、日志器组装成可运行的 HTTP 服务。
//
// 装配顺序（不可随意调整）：
//  1. 构建注册表并校验，失败即拒绝启动；
//  2. 创建 Hertz 实例并设置协议层参数；
//  3. 注册全局中间件（顺序见 middleware 包注释）；
//  4. 注册四类家族路由与健康检查；
//  5. 注册兜底处理器。
//
// 依赖显式传递，不引入全局单例：后续接入 Huobao 客户端、鉴权器、限流计数器时，
// 只需在这里增删字段，调用方无感。
package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"

	"newx-gateway/internal/config"
	"newx-gateway/internal/contracts"
	"newx-gateway/internal/discovery"
	"newx-gateway/internal/middleware"
	"newx-gateway/internal/proxy"
	"newx-gateway/internal/registry"
	"newx-gateway/internal/transport"
	"newx-gateway/internal/transport/admin"
	"newx-gateway/internal/transport/health"
	"newx-gateway/internal/transport/mcp"
	"newx-gateway/internal/transport/openapi"
	"newx-gateway/internal/transport/web"
)

// App 是装配完成的入口服务。
type App struct {
	Hertz    *server.Hertz
	Config   *config.Config
	Logger   *slog.Logger
	Registry *registry.Registry

	scopeGuard    *middleware.ScopeGuard
	limiter       *middleware.Limiter
	modules       []string
	discoveryStop context.CancelFunc
}

// New 构建入口服务。任何装配失败都会返回错误，由进程入口以非零码退出。
func New(cfg *config.Config, logger *slog.Logger) (*App, error) {
	if logger == nil {
		logger = slog.Default()
	}

	reg, err := registry.New(registry.SpecsFromConfig(cfg.Apps))
	if err != nil {
		return nil, err
	}

	resolver, discoveryStop, err := buildResolver(cfg, logger)
	if err != nil {
		return nil, err
	}

	h := server.New(
		server.WithHostPorts(cfg.HTTPAddr),
		server.WithExitWaitTime(cfg.ExitWaitTime),
		// 协议层体积上限与中间件的语义层校验构成两道防线，分工不同：
		//   - 协议层用「上传通道上限」兜底，避免恶意超大 body 被读入内存；
		//   - 语义层（middleware.BodyLimit）按普通通道上限给出统一的 40002 错误体。
		// 若协议层直接压到普通通道上限，超限请求会在协议层被截断为框架自带的纯文本响应，
		// 破坏「入口错误响应形状统一」这条约束。
		server.WithMaxRequestBodySize(int(cfg.MaxBodyBytesUpload)),
		// 协议层只设置「跨全部类别都安全」的宽松上限：按类别区分的预算属于
		// 转发阶段的能力（见 config.Timeouts 的注释），过早收紧会掐断长耗时生成请求。
		server.WithReadTimeout(protocolReadTimeout(cfg)),
		server.WithWriteTimeout(protocolWriteTimeout(cfg)),
		server.WithIdleTimeout(cfg.Timeouts.Read),
		// 关闭框架自身的路由表打印，启动信息由本层结构化输出，避免污染 JSON 日志。
		server.WithDisablePrintRoute(true),
	)

	// 全局中间件：顺序即语义，逐项理由见 middleware 包注释。
	h.Use(
		middleware.Recover(logger),
		middleware.Tracing(),
		middleware.RequestLogger(logger),
		middleware.HeaderGuard(),
		middleware.BodyLimit(cfg.MaxBodyBytes),
	)

	// 家族级中间件：鉴权 → 范围校验 → 限流。本阶段三者均为空位（恒放行），
	// 接入签发方与计数逻辑后无需改动路由注册。
	scopeGuard := middleware.NewScopeGuard()
	limiter := middleware.NewLimiter(cfg.Rates, cfg.MCPEnabled, cfg.OpenAPIEnabled)
	authMiddleware := middleware.Authenticator(logger)
	familyChain := func(bucket string) []app.HandlerFunc {
		return []app.HandlerFunc{
			authMiddleware,
			scopeGuard.Middleware(),
			limiter.Middleware(bucket),
		}
	}

	webModule := web.New()
	webModule.Middlewares = familyChain(middleware.BucketWeb)

	adminModule := admin.New()
	adminModule.Middlewares = familyChain(middleware.BucketAdmin)

	mcpModule := mcp.New(cfg.MCPEnabled)
	mcpModule.Middlewares = familyChain(middleware.BucketMCP)

	openAPIModule := openapi.New(cfg.OpenAPIEnabled)
	openAPIModule.Middlewares = familyChain(middleware.BucketOpenAPI)

	healthModule := health.New(cfg.Version, cfg.Env, cfg.ReadyzProbeDownstream, downstreams(cfg))

	deps := transport.Deps{
		Config:    cfg,
		Registry:  reg,
		Logger:    logger,
		Discovery: resolver,
		Proxy:     proxy.New(proxyOptions(cfg)),
	}
	modules := transport.Register(h, deps,
		webModule, adminModule, mcpModule, openAPIModule, healthModule,
	)

	fallback := transport.NoRouteHandler()
	// 未匹配路由与方法不匹配都返回统一的「路由不存在」，避免暴露路由表结构。
	h.NoRoute(fallback)
	h.NoMethod(fallback)

	return &App{
		Hertz:         h,
		Config:        cfg,
		Logger:        logger,
		Registry:      reg,
		scopeGuard:    scopeGuard,
		limiter:       limiter,
		modules:       modules,
		discoveryStop: discoveryStop,
	}, nil
}

// Run 启动监听并阻塞，直到进程关闭或发生错误。
func (a *App) Run() error { return a.Hertz.Run() }

// Shutdown 优雅停止接收新请求并等待在途请求结束。
func (a *App) Shutdown(ctx context.Context) error {
	if a.discoveryStop != nil {
		a.discoveryStop()
	}
	return a.Hertz.Shutdown(ctx)
}

// Modules 返回已注册的路由模块名，用于启动日志与自检。
func (a *App) Modules() []string {
	result := make([]string, len(a.modules))
	copy(result, a.modules)
	return result
}

// ScopeGuard 暴露范围守卫，便于后续在装配层开关强制校验。
func (a *App) ScopeGuard() *middleware.ScopeGuard { return a.scopeGuard }

// Limiter 暴露限流器，便于后续在装配层开关计数与拦截。
func (a *App) Limiter() *middleware.Limiter { return a.limiter }

// protocolReadTimeout 取读取类预算的最大值，作为协议层上限。
func protocolReadTimeout(cfg *config.Config) time.Duration {
	return maxDuration(cfg.Timeouts.Read, cfg.Timeouts.Upload)
}

// protocolWriteTimeout 取写入类预算的最大值，作为协议层上限。
// 生成类与上传类请求可能持续数分钟到数十分钟，协议层必须留足空间。
func protocolWriteTimeout(cfg *config.Config) time.Duration {
	return maxDuration(
		cfg.Timeouts.Write,
		cfg.Timeouts.Generate,
		cfg.Timeouts.Upload,
		cfg.Timeouts.Static,
		cfg.Timeouts.MCP,
	)
}

func maxDuration(values ...time.Duration) time.Duration {
	var result time.Duration
	for _, value := range values {
		if value > result {
			result = value
		}
	}
	return result
}

// downstreams 把应用注册表转换为健康检查的探测目标。
func downstreams(cfg *config.Config) []health.Downstream {
	result := make([]health.Downstream, 0, len(cfg.Apps))
	for _, item := range cfg.Apps {
		result = append(result, health.Downstream{AppCode: item.Code, BaseURL: item.BaseURL})
	}
	return result
}

// buildResolver 构建服务解析器：静态地址与 Nacos 发现按应用共存。
//
// 返回的取消函数用于停止 Nacos 后台刷新协程，由 Shutdown 调用。
func buildResolver(cfg *config.Config, logger *slog.Logger) (discovery.Resolver, context.CancelFunc, error) {
	multi := discovery.NewMulti()

	staticAddresses := make(map[string][]string)
	nacosServices := make([]string, 0, len(cfg.Apps))
	for _, app := range cfg.Apps {
		if app.Service != "" {
			nacosServices = append(nacosServices, app.Service)
			continue
		}
		if address := hostPortOf(app.BaseURL); address != "" {
			staticAddresses[app.Code] = append(staticAddresses[app.Code], address)
		}
	}

	if len(staticAddresses) > 0 {
		staticResolver, err := discovery.NewStaticResolver(staticAddresses)
		if err != nil {
			return nil, nil, err
		}
		for _, service := range staticResolver.Services() {
			multi.Add(service, staticResolver)
		}
	}

	if len(nacosServices) > 0 {
		if !cfg.Discovery.Enabled {
			return nil, nil, fmt.Errorf("APPS 使用了 nacos:// 服务，但 DISCOVERY 未配置")
		}
		nacosResolver, err := discovery.NewNacosResolver(discovery.NacosConfig{
			Addr:      cfg.Discovery.Addr,
			Namespace: cfg.Discovery.Namespace,
			Group:     cfg.Discovery.Group,
			Refresh:   cfg.Discovery.Refresh,
			Logger:    logger,
		})
		if err != nil {
			return nil, nil, err
		}
		for _, service := range nacosServices {
			multi.Add(service, nacosResolver)
		}

		ctx, cancel := context.WithCancel(context.Background())
		nacosResolver.Start(ctx)
		return multi, cancel, nil
	}

	return multi, nil, nil
}

// proxyOptions 依据配置构造转发参数。
//
// 预算按路由家族近似划分：生成类与上传类请求可能持续数分钟到数十分钟，
// 因此 Web 家族取写入/生成/上传三类的最大值，避免长请求被误杀。
// 待路由元数据就绪后，这里会细化到「读/写/生成/上传」四类请求各自独立预算。
func proxyOptions(cfg *config.Config) proxy.Options {
	return proxy.Options{
		DialTimeout: 2 * time.Second,
		Budgets: map[string]time.Duration{
			contracts.FamilyWeb:     maxDuration(cfg.Timeouts.Write, cfg.Timeouts.Generate, cfg.Timeouts.Upload),
			contracts.FamilyAdmin:   cfg.Timeouts.Write,
			contracts.FamilyMCP:     cfg.Timeouts.MCP,
			contracts.FamilyOpenAPI: cfg.Timeouts.Write,
		},
		DefaultBudget:       cfg.Timeouts.Write,
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 64,
		TrustProxyHeaders:   cfg.TrustProxyHeaders,
	}
}

// hostPortOf 提取 URL 的主机部分（host:port）。
func hostPortOf(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	slash := strings.Index(rawURL, "://")
	if slash < 0 {
		return ""
	}
	rest := rawURL[slash+3:]
	if at := strings.IndexAny(rest, "/?#"); at >= 0 {
		rest = rest[:at]
	}
	return rest
}
