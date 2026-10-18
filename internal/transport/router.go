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
	// Breaker 记录实例失败并临时摘除坏实例；为空表示不启用熔断。
	Breaker *lb.Breaker
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
		// 同时必须使用原始（仍带百分号编码）路径，否则含中文或 %2F 的 URL
		// 会被解码后重新编码，导致下游路径匹配错位。
		appCode, rest := splitAppCode(m.Prefix, originalPath(c), m.Versioned)
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

		// 摘除熔断中的实例。全部实例都处于熔断状态时直接快速失败：
		// 继续把请求发给已知故障的下游只会白占连接、拖慢客户端，
		// 而熔断冷却结束后 Allow 会自动放行试探请求，流量自行恢复。
		if deps.Breaker != nil {
			available := deps.Breaker.Available(candidates)
			if len(available) == 0 {
				httpx.WriteError(c, contracts.CodeDownstreamRefused)
				return
			}
			candidates = available
		}

		if len(candidates) == 0 {
			httpx.WriteError(c, contracts.CodeDownstreamRefused)
			return
		}

		target := proxy.Target{
			Scheme:   candidates[0].Scheme,
			Host:     candidates[0].Address(),
			Path:     joinPath(registered.UpstreamPath, rest),
			RawQuery: string(c.Request.URI().QueryString()),
		}

		if err := m.forward(ctx, deps, c, target, candidates); err != nil {
			var proxyErr *proxy.Error
			if errors.As(err, &proxyErr) {
				httpx.WriteError(c, proxyErr.Code)
				return
			}
			httpx.WriteError(c, contracts.CodeDownstreamBad)
			return
		}

		// 下游 5xx：响应体仍原样透传（客户端的错误处理不被入口破坏），
		// 但错误码归类记为 50202，供日志与告警识别；
		// 同时计入实例失败，让熔断器能把持续报错的下游摘掉。
		upstream := httpx.String(c, contracts.CtxUpstreamHost)
		switch {
		case c.Response.StatusCode() >= 500:
			c.Set(contracts.CtxDownstreamErrorCode, int(contracts.CodeDownstreamBad))
			if upstream != "" {
				deps.Breaker.RecordFailure(upstream)
			}
		case upstream != "":
			deps.Breaker.RecordSuccess(upstream)
		}
	}
}

// forward 执行转发，并在必要时重试。
//
// 重试策略（设计文档 §9）：
//   - 仅幂等方法（GET/HEAD/OPTIONS）可重试，且最多 1 次；
//   - 仅传输层失败（拨号失败 50201、超时 50401）可重试，此时响应尚未开始写出，
//     重试不会产生重复响应；
//   - 重试必须换一个实例，否则只是把同样的失败再演一遍；
//   - 写方法与生成类请求一律不自动重试，由客户端带幂等键自行决定。
func (m FamilyModule) forward(ctx context.Context, deps Deps, c *app.RequestContext, target proxy.Target, candidates []discovery.Instance) error {
	maxAttempts := 1
	if deps.Proxy.CanRetry(string(c.Method()), string(c.Path())) {
		maxAttempts = 2
	}

	excluded := make(map[string]bool)
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		chosen, ok := pickExcluding(candidates, excluded)
		if !ok {
			break
		}

		target.Scheme = chosen.Scheme
		target.Host = chosen.Address()
		c.Set(contracts.CtxUpstreamHost, chosen.Address())

		lastErr = deps.Proxy.Forward(ctx, c, target, m.Family)

		if lastErr != nil {
			// 传输层失败：记入实例失败，供熔断器摘除。
			deps.Breaker.RecordFailure(chosen.Address())
		}

		var proxyErr *proxy.Error
		if !errors.As(lastErr, &proxyErr) {
			return lastErr
		}
		// 只有「根本连不上」和「超时」值得换实例再试一次；
		// 下游已给出响应（含错误响应）时不重试。
		if proxyErr.Code != contracts.CodeDownstreamRefused && proxyErr.Code != contracts.CodeDownstreamTimeout {
			return lastErr
		}
		if attempt >= maxAttempts {
			break
		}
		excluded[chosen.Address()] = true
	}

	return lastErr
}

// pickExcluding 从候选集中选一个未被排除的实例。
//
// 若排除后没有剩余实例（单实例部署的常见情况），回退到全量候选集：
// 连接级抖动往往重连一次就能恢复，此时「换实例」没有意义，
// 但不重试又会把瞬时失败直接暴露给客户端。
func pickExcluding(candidates []discovery.Instance, excluded map[string]bool) (discovery.Instance, bool) {
	remaining := make([]discovery.Instance, 0, len(candidates))
	for _, instance := range candidates {
		if excluded[instance.Address()] {
			continue
		}
		remaining = append(remaining, instance)
	}
	if len(remaining) == 0 {
		remaining = candidates
	}
	return lb.Pick(remaining)
}

// originalPath 返回客户端原始请求路径（保留百分号编码）。
func originalPath(c *app.RequestContext) string {
	if raw := c.Request.URI().PathOriginal(); len(raw) > 0 {
		return string(raw)
	}
	return string(c.Path())
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
