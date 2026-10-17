// Package transport 是入口的协议层：只做路由、参数解析与响应写出，
// 不包含业务规则，也不直接访问下游。
//
// 四类家族各注册一条通配路由，命中后由家族处理器解析 app_code 并校验注册表。
// 之所以用通配而不是逐条注册下游路由：下游接口会持续增加，
// 逐条注册等于在入口维护第二份路由真相，长期必然漂移。
package transport

import (
	"context"
	"log/slog"
	"net/url"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"

	"newx-gateway/internal/config"
	"newx-gateway/internal/contracts"
	"newx-gateway/internal/httpx"
	"newx-gateway/internal/registry"
)

// Deps 是处理器可用的依赖集合。显式传递，不使用全局单例，
// 便于后续把 Huobao 客户端、限流器、鉴权器逐个接进来。
type Deps struct {
	Config   *config.Config
	Registry *registry.Registry
	Logger   *slog.Logger
}

// Module 是一个可注册的路由模块。
type Module interface {
	// Name 返回模块名，用于启动日志。
	Name() string
	// Register 把路由挂到 Hertz 实例上。
	Register(h *server.Hertz, deps Deps)
}

// Register 按顺序注册全部模块。
func Register(h *server.Hertz, deps Deps, modules ...Module) []string {
	names := make([]string, 0, len(modules))
	for _, module := range modules {
		if module == nil {
			continue
		}
		module.Register(h, deps)
		names = append(names, module.Name())
	}
	return names
}

// FamilyModule 是四类路由家族的通用实现。
//
// 本阶段处理器只完成「解析应用 → 校验注册表 → 返回未实现」三步：
// 反代接入后替换 handler 内部实现即可，路由注册、错误码与日志字段都保持不变。
type FamilyModule struct {
	// Family 是家族标识（web/admin/mcp/openapi），同时用于日志归类与限流分桶。
	Family    string
	Prefix    string
	Versioned bool
	// Enabled 为 false 时不注册路由，请求会落到兜底处理器并返回「路由不存在」。
	Enabled bool
	// Middlewares 是家族级中间件（鉴权、范围校验、限流），由装配层注入，
	// 顺序即调用顺序，最后总是家族处理器。
	Middlewares []app.HandlerFunc
}

// Name 实现 Module。
func (m FamilyModule) Name() string { return m.Family }

// Register 实现 Module：注册家族前缀下的全部路径形态。
func (m FamilyModule) Register(h *server.Hertz, deps Deps) {
	if !m.Enabled {
		return
	}

	base := m.Prefix + "/:app_code"
	paths := []string{base}
	if m.Versioned {
		paths = append(paths, base+"/v1", base+"/v1/*rest")
	} else {
		paths = append(paths, base+"/*rest")
	}

	chain := make([]app.HandlerFunc, 0, len(m.Middlewares)+1)
	chain = append(chain, m.Middlewares...)
	chain = append(chain, m.handler(deps))

	for _, path := range paths {
		h.Any(path, chain...)
	}
}

// handler 是家族占位处理器。
func (m FamilyModule) handler(deps Deps) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		httpx.SetRoute(c, m.Family)

		appCode := c.Param("app_code")
		registered, found := deps.Registry.Lookup(appCode)
		if !found {
			// 未注册的应用一律拒绝：避免未知前缀被透传到任意后端。
			httpx.WriteError(c, contracts.CodeAppNotFound)
			return
		}

		c.Set(contracts.CtxAppCode, registered.Code)
		c.Set(contracts.CtxDownstream, registered.Code)
		c.Request.Header.Set(contracts.HeaderAppCode, registered.Code)
		if host := hostOf(registered.BaseURL); host != "" {
			c.Set(contracts.CtxUpstreamHost, host)
		}

		// 反代接入点（后续迭代）：在此调用 clients/huobao 之类的下游客户端，
		// 把下游响应（状态码、响应头、流式 body）原样写回；
		// 拨号失败映射 50201、超时映射 50401、下游 5xx 归一为 50202。
		// 本阶段下游尚未接入，统一返回「未实现」，语义稳定且可被测试断言。
		httpx.WriteError(c, contracts.CodeNotImplemented)
	}
}

// hostOf 提取 URL 的主机部分，用于日志字段 upstream_host。
func hostOf(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Host
}
