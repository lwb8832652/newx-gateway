package middleware

import (
	"context"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"

	"newx-gateway/internal/contracts"
	"newx-gateway/internal/httpx"
)

// Rule 描述一条路由级授权范围声明。
//
// 空字段表示通配：Method 为空匹配所有方法，PathPrefix 为空匹配所有路径。
type Rule struct {
	Method     string
	PathPrefix string
	Scope      string
}

// ScopeGuard 是授权范围校验中间件。
//
// 本阶段 enabled 恒为 false：身份体系尚未接入，若此时启用范围校验，
// 所有请求都会因「无主体」被拒，脚手架将无法联调。
// 规则登记能力现在就具备了，接入鉴权时只需把 enabled 打开。
type ScopeGuard struct {
	enabled bool
	rules   []Rule
}

// NewScopeGuard 创建范围守卫，默认不启用强制校验。
func NewScopeGuard() *ScopeGuard { return &ScopeGuard{} }

// SetEnabled 开关强制校验。接入鉴权中间件后应设为 true。
func (g *ScopeGuard) SetEnabled(enabled bool) {
	if g != nil {
		g.enabled = enabled
	}
}

// Enabled 报告强制校验是否已启用。
func (g *ScopeGuard) Enabled() bool { return g != nil && g.enabled }

// Allow 登记规则。
func (g *ScopeGuard) Allow(rules ...Rule) {
	if g == nil {
		return
	}
	g.rules = append(g.rules, rules...)
}

// Rules 返回已登记规则的副本。
func (g *ScopeGuard) Rules() []Rule {
	if g == nil {
		return nil
	}
	result := make([]Rule, len(g.rules))
	copy(result, g.rules)
	return result
}

// Match 返回与该请求匹配的第一条规则。
func (g *ScopeGuard) Match(method, path string) (Rule, bool) {
	if g == nil {
		return Rule{}, false
	}
	for _, rule := range g.rules {
		if rule.Method != "" && !strings.EqualFold(rule.Method, method) {
			continue
		}
		if rule.PathPrefix != "" && !strings.HasPrefix(path, rule.PathPrefix) {
			continue
		}
		return rule, true
	}
	return Rule{}, false
}

// Middleware 返回范围校验中间件。
// 未启用或未命中规则时直接放行；命中规则但缺少主体时返回 40101，
// 主体存在但范围不足时返回 40103。
func (g *ScopeGuard) Middleware() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		if !g.Enabled() {
			c.Next(ctx)
			return
		}

		rule, matched := g.Match(string(c.Method()), string(c.Path()))
		if !matched {
			c.Next(ctx)
			return
		}

		principal := httpx.String(c, contracts.CtxPrincipalID)
		if principal == "" {
			httpx.WriteError(c, contracts.CodeUnauthorized)
			return
		}
		if !hasScope(httpx.String(c, contracts.CtxUserScope), rule.Scope) {
			httpx.WriteError(c, contracts.CodeScopeInsufficient)
			return
		}

		c.Next(ctx)
	}
}

// hasScope 判断逗号分隔的范围列表是否包含目标范围。
// 目标范围为空时视为无需范围。
func hasScope(granted, required string) bool {
	required = strings.TrimSpace(required)
	if required == "" {
		return true
	}
	for _, item := range strings.Split(granted, ",") {
		if strings.EqualFold(strings.TrimSpace(item), required) {
			return true
		}
	}
	return false
}
