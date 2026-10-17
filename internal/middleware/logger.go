package middleware

import (
	"context"
	"log/slog"
	"time"

	"github.com/cloudwego/hertz/pkg/app"

	"newx-gateway/internal/contracts"
	"newx-gateway/internal/httpx"
	"newx-gateway/internal/observability"
)

// RequestLogger 在请求结束后输出单行结构化日志。
//
// 不记录的内容（硬性约束）：Authorization/Cookie 等凭据、请求与响应正文。
// 需要排障时依赖 request_id 去下游日志取详情，而不是把正文复制到入口日志。
func RequestLogger(logger *slog.Logger) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		if logger == nil {
			c.Next(ctx)
			return
		}

		started := time.Now()
		c.Next(ctx)

		entry := BuildRequestLog(c, started)
		entry.Log(logger)
	}
}

// BuildRequestLog 汇总一次请求的可观测字段。
// 独立成函数便于单测直接断言字段，不需要起真实服务。
func BuildRequestLog(c *app.RequestContext, started time.Time) observability.RequestLog {
	entry := observability.RequestLog{
		RequestID:    httpx.RequestID(c),
		TraceID:      httpx.TraceID(c),
		AppCode:      httpx.AppCode(c),
		Route:        httpx.Route(c),
		Method:       string(c.Method()),
		Downstream:   httpx.Downstream(c),
		UpstreamHost: httpx.UpstreamHost(c),
		Status:       c.Response.StatusCode(),
		LatencyMS:    float64(time.Since(started).Microseconds()) / 1000.0,
		RespBytes:    len(c.Response.Body()),
		PrincipalID:  httpx.String(c, contracts.CtxPrincipalID),
		ErrorCode:    httpx.ErrorCode(c),
		ErrMsg:       httpx.ErrorMessage(c),
	}

	// 请求体大小：Content-Length 可能为 -1（分块传输），负数一律按 0 处理。
	if length := c.Request.Header.ContentLength(); length > 0 {
		entry.ReqBytes = int64(length)
	}

	// code 与 error_code 在本阶段同源：入口写出的业务错误码。
	// 接入反代后下游错误会单独归类，届时两者会分化。
	entry.Code = entry.ErrorCode

	if entry.Route == "" {
		entry.Route = contracts.FamilyUnknown
	}
	return entry
}
