// Package transport 是入口的协议层：只做路由、参数解析、上游选择与响应写出，
// 不包含业务规则。
//
// 四类家族各注册一条通配路由，命中后由家族处理器解析 app_code、校验注册表、
// 解析下游实例、按灰度规则筛选、负载均衡选取，最后转发并原样回写响应。
//
// 之所以用通配而不是逐条注册下游路由：下游接口会持续增加，
// 逐条注册等于在入口维护第二份路由真相，长期必然漂移。
package transport

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"

	"newx-gateway/internal/config"
	"newx-gateway/internal/contracts"
	"newx-gateway/internal/discovery"
	"newx-gateway/internal/httpx"
	"newx-gateway/internal/lb"
	"newx-gateway/internal/proxy"
	"newx-gateway/internal/registry"
)

// Deps 是处理器可用的依赖集合。显式传递，不使用全局单例，
// 便于后续把鉴权器、限流器逐个接进来。
type Deps struct {
	Config   *config.Config
	Registry *registry.Registry
	Logger   *slog.Logger
	// Discovery 解析 app_code 对应的下游实例（静态地址或注册中心）。
	Discovery discovery.Resolver
	// Proxy 负责把请求转发到选中的实例。
	Proxy *proxy.Proxy
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

// handler 是家族处理器：解析应用 → 选实例 → 灰度筛选 → 转发 → 回写响应。
func (m FamilyModule) handler(deps Deps) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		httpx.SetRoute(c, m.Family)

		// 应用编码与剩余路径一律从 URL 解析，不依赖路由参数：
		// 框架的通配参数在不同路径形态下取值不一致，而入口路径规则是自己定的，
		// 自己解析才能保证「同一条规则、同一种结果」。
		appCode, rest := splitAppCode(m.Prefix, string(c.Path()), m.Versioned)
		if appCode == "" {
			httpx.WriteError(c, contracts.CodeRouteNotFound)
			return
		}

		registered, found := deps.Registry.Lookup(appCode)
		if !found {
			// 未注册的应用一律拒绝：避免未知前缀被透传到任意后端。
			httpx.WriteError(c, contracts.CodeAppNotFound)
			return
		}

		c.Set(contracts.CtxAppCode, registered.Code)
		c.Set(contracts.CtxDownstream, registered.Code)
		c.Request.Header.Set(contracts.HeaderAppCode, registered.Code)

		if !registered.Routable() || deps.Discovery == nil || deps.Proxy == nil {
			// 已注册但下游来源缺失：与「未实现」区分，表示配置不完整。
			httpx.WriteError(c, contracts.CodeDownstreamRefused)
			return
		}

		instances, err := deps.Discovery.Resolve(ctx, registered.Code)
		if err != nil {
			if deps.Logger != nil {
				deps.Logger.Warn("解析下游实例失败",
					"app_code", registered.Code,
					"request_id", httpx.RequestID(c),
					"err", err,
				)
			}
			httpx.WriteError(c, contracts.CodeDownstreamRefused)
			return
		}

		// 灰度筛选：请求头 version / tag，语义与 yudao 网关保持一致。
		requestVersion := c.Request.Header.Get("version")
		requestTag := c.Request.Header.Get("tag")
		candidates, fellBack := lb.FilterGray(instances, requestVersion, requestTag)
		if fellBack && deps.Logger != nil {
			deps.Logger.Warn("灰度筛选无匹配实例，已回退全量",
				"app_code", registered.Code,
				"version", requestVersion,
				"tag", requestTag,
			)
		}

		chosen, ok := lb.Pick(candidates)
		if !ok {
			httpx.WriteError(c, contracts.CodeDownstreamRefused)
			return
		}

		c.Set(contracts.CtxUpstreamHost, chosen.Address())

		target := proxy.Target{
			Scheme:   chosen.Scheme,
			Host:     chosen.Address(),
			Path:     joinPath(registered.UpstreamPath, rest),
			RawQuery: string(c.Request.URI().QueryString()),
		}

		if err := deps.Proxy.Forward(ctx, c, target, m.Family); err != nil {
			var proxyErr *proxy.Error
			if errors.As(err, &proxyErr) {
				httpx.WriteError(c, proxyErr.Code)
				return
			}
			httpx.WriteError(c, contracts.CodeDownstreamBad)
			return
		}

		// 下游 5xx：响应体仍原样透传（客户端的错误处理不被入口破坏），
		// 但错误码归类记为 50202，供日志与告警识别。
		if c.Response.StatusCode() >= 500 {
			c.Set(contracts.CtxDownstreamErrorCode, int(contracts.CodeDownstreamBad))
		}
	}
}

// splitAppCode 从请求路径中解析应用编码与剩余路径。
//
//	/web/huobao/v1/dramas → ("huobao", "/dramas")
//	/web/huobao           → ("huobao", "")
//	/mcp/huobao/tools     → ("huobao", "/tools")
//
// 带版本段的家族会再剥掉前导的 /v1；yudao 这类没有版本概念的下游不受影响。
func splitAppCode(prefix, fullPath string, versioned bool) (string, string) {
	trimmed := strings.TrimPrefix(fullPath, prefix+"/")
	if trimmed == "" || trimmed == fullPath {
		return "", ""
	}

	code, remainder, found := strings.Cut(trimmed, "/")
	code = strings.TrimSpace(code)
	if code == "" {
		return "", ""
	}
	if !found {
		return code, ""
	}

	rest := "/" + remainder
	if versioned {
		rest = strings.TrimPrefix(rest, "/v1")
	}
	return code, rest
}

// joinPath 把上游路径前缀与剩余路径拼成完整上游路径，避免出现 // 与空路径。
func joinPath(prefix, rest string) string {
	prefix = strings.TrimSuffix(prefix, "/")
	rest = strings.TrimSpace(rest)
	if rest != "" && !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}

	switch {
	case prefix == "" && rest == "":
		return "/"
	case prefix == "":
		return rest
	case rest == "" || rest == "/":
		return prefix
	default:
		return prefix + rest
	}
}
