package middleware

import (
	"context"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"

	"newx-gateway/internal/contracts"
	"newx-gateway/internal/observability"
)

// requestIDMaxLen 限制客户端传入的请求标识长度，避免超长值污染日志。
const requestIDMaxLen = 64

// Tracing 解析或生成请求标识与链路标识，写入上下文并注入下游请求头。
//
// 客户端传入的 traceparent 合法时沿用（保持跨系统链路连续），
// 不合法或缺失时生成新的链路上下文——入口不因此拒绝请求。
func Tracing() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		requestID := sanitizeRequestID(c.Request.Header.Get(contracts.HeaderRequestID))
		if requestID == "" {
			requestID = observability.NewRequestID()
		}

		trace, ok := observability.ParseTraceParent(c.Request.Header.Get(contracts.HeaderTraceParent))
		if !ok {
			trace = observability.NewTraceContext()
		}

		c.Set(contracts.CtxRequestID, requestID)
		c.Set(contracts.CtxTraceID, trace.TraceID)

		// 注入下游请求头：下游据此串链，无需再解析入口的日志。
		c.Request.Header.Set(contracts.HeaderRequestID, requestID)
		c.Request.Header.Set(contracts.HeaderTraceID, trace.TraceID)
		c.Request.Header.Set(contracts.HeaderTraceParent, trace.Header())

		// 响应头回带请求标识，前端报错时可直接对照后端日志。
		c.Response.Header.Set(contracts.HeaderRequestID, requestID)

		ctx = observability.WithRequestID(ctx, requestID)
		ctx = observability.WithTraceID(ctx, trace.TraceID)

		c.Next(ctx)
	}
}

// sanitizeRequestID 只接受可安全写入日志的字符集；不合法时返回空串，由调用方重新生成。
func sanitizeRequestID(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" || len(value) > requestIDMaxLen {
		return ""
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return ""
		}
	}
	return value
}
