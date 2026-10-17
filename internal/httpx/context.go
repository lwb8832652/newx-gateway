// Package httpx 提供基于 Hertz RequestContext 的读写小工具。
//
// 存在的意义：中间件层与路由层都需要「读取本次请求的元信息」和「写出统一错误」，
// 若任一层各自实现，就会出现两套语义。这里集中一次，contracts 保持零框架依赖。
package httpx

import (
	"github.com/cloudwego/hertz/pkg/app"

	"newx-gateway/internal/contracts"
)

// String 从 Hertz 上下文中读取字符串键。
func String(c *app.RequestContext, key string) string {
	if c == nil {
		return ""
	}
	value, exists := c.Get(key)
	if !exists {
		return ""
	}
	text, _ := value.(string)
	return text
}

// Int 从 Hertz 上下文中读取整型键，缺失或类型不符时返回 0。
func Int(c *app.RequestContext, key string) int {
	if c == nil {
		return 0
	}
	value, exists := c.Get(key)
	if !exists {
		return 0
	}
	number, _ := value.(int)
	return number
}

// RequestID 返回本次请求的标识。
func RequestID(c *app.RequestContext) string { return String(c, contracts.CtxRequestID) }

// TraceID 返回本次请求的链路标识。
func TraceID(c *app.RequestContext) string { return String(c, contracts.CtxTraceID) }

// AppCode 返回本次请求命中的应用编码。
func AppCode(c *app.RequestContext) string { return String(c, contracts.CtxAppCode) }

// Route 返回本次请求命中的路由家族（web/admin/mcp/openapi/health/unknown）。
func Route(c *app.RequestContext) string { return String(c, contracts.CtxRoute) }

// Downstream 返回本次请求的下游应用编码。
func Downstream(c *app.RequestContext) string { return String(c, contracts.CtxDownstream) }

// UpstreamHost 返回本次请求的下游主机，仅用于日志。
func UpstreamHost(c *app.RequestContext) string { return String(c, contracts.CtxUpstreamHost) }

// ErrorCode 返回入口已写出的业务错误码，未写出错误时为 0。
func ErrorCode(c *app.RequestContext) int { return Int(c, contracts.CtxErrorCode) }

// ErrorMessage 返回入口已写出的错误说明。
func ErrorMessage(c *app.RequestContext) string { return String(c, contracts.CtxErrMsg) }

// SetRoute 记录本次请求命中的路由家族。处理器在解析出家族后调用。
func SetRoute(c *app.RequestContext, family string) {
	if c == nil {
		return
	}
	c.Set(contracts.CtxRoute, family)
}
