// Package legacy 提供 yudao 旧路径兼容：把 /admin-api/{module}/**、
// /app-api/{module}/** 这类「路径即服务」的规则映射到应用注册表。
//
// 设计取舍：
//   - 映射必须显式配置（LEGACY_ROUTES），不从 app_code 猜模块名。
//     猜错会把请求静默送到错误服务，比直接 404 更难排查；
//   - 命中后保留原始路径原样转发，不做任何改写，行为与 yudao-gateway
//     的 Path 断言一致；
//   - 与新区族共用 transport.Serve 的实例解析、灰度、熔断与错误分类，
//     避免出现两套语义。
package legacy

import (
	"context"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"

	"newx-gateway/internal/config"
	"newx-gateway/internal/contracts"
	"newx-gateway/internal/httpx"
	"newx-gateway/internal/registry"
	"newx-gateway/internal/transport"
)

// 家族名固定为 legacy，用于请求日志归类。
const family = "legacy"

// Module 是旧路径兼容模块，一条配置对应一组路由。
type Module struct {
	// Routes 来自 LEGACY_ROUTES，已在启动期校验过前缀与应用编码。
	Routes []config.LegacyRoute
	// Middlewares 与新区族共用同一套家族级中间件（鉴权、范围校验、限流）。
	Middlewares []app.HandlerFunc
}

// New 构建旧路径兼容模块。
func New(routes []config.LegacyRoute) Module { return Module{Routes: routes} }

// Name 实现 transport.Module。
func (m Module) Name() string { return family }

// Register 实现 transport.Module：为每条映射注册「精确路径 + 通配子路径」。
//
// 与 yudao 的 Path=/admin-api/system/** 等价；精确路径覆盖
// /admin-api/system 这种不带尾部斜杠的请求。
func (m Module) Register(h *server.Hertz, deps transport.Deps) {
	for _, route := range m.Routes {
		registered, found := deps.Registry.Lookup(route.AppCode)
		if !found {
			// 配置层已经校验过应用存在；这里跳过只是防御性处理。
			continue
		}

		chain := make([]app.HandlerFunc, 0, len(m.Middlewares)+1)
		chain = append(chain, m.Middlewares...)
		chain = append(chain, m.handler(deps, registered))
		h.Any(route.Prefix, chain...)
		h.Any(route.Prefix+"/*rest", chain...)
	}
}

// handler 把请求原样转发到映射的应用。
func (m Module) handler(deps transport.Deps, registered registry.App) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		httpx.SetRoute(c, family)
		// 旧路径语义：路径即服务，原样转发，不做任何改写。
		transport.Serve(ctx, deps, c, registered, transport.OriginalPath(c), family)
	}
}

// Fallback 返回「app_code 未注册」时的兜底处理器，供各家族模块挂载。
//
// 为什么需要它：/admin/**、/mcp/message 这类旧路径会先被
// /admin/{app_code}、/mcp/{app_code} 参数段路由接住，静态前缀路由没有机会匹配。
// 兜底时按原始路径重新做一次最长前缀匹配，命中旧前缀就原样转发；
// 仍然没有匹配则保持原有的 40402 语义。
func (m Module) Fallback(deps transport.Deps) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		path := transport.OriginalPath(c)
		route, ok := m.match(path)
		if !ok {
			httpx.WriteError(c, contracts.CodeAppNotFound)
			return
		}
		registered, found := deps.Registry.Lookup(route.AppCode)
		if !found {
			httpx.WriteError(c, contracts.CodeAppNotFound)
			return
		}

		httpx.SetRoute(c, family)
		transport.Serve(ctx, deps, c, registered, path, family)
	}
}

// match 按最长前缀匹配旧路径映射，保证 /admin-api/system 优先于 /admin-api。
func (m Module) match(path string) (config.LegacyRoute, bool) {
	var best config.LegacyRoute
	found := false
	for _, route := range m.Routes {
		if path != route.Prefix && !strings.HasPrefix(path, route.Prefix+"/") {
			continue
		}
		if !found || len(route.Prefix) > len(best.Prefix) {
			best, found = route, true
		}
	}
	return best, found
}
