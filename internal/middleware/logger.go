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
		RespBytes:    responseSize(c),
		PrincipalID:  httpx.String(c, contracts.CtxPrincipalID),
		ErrorCode:    httpx.ErrorCode(c),
		ErrMsg:       httpx.ErrorMessage(c),
	}

	// 请求体大小：Content-Length 可能为 -1（分块传输），负数一律按 0 处理。
	if length := c.Request.Header.ContentLength(); length > 0 {
		entry.ReqBytes = int64(length)
	}

	// code 记录入口自身产生的错误码；error_code 记录本次请求的错误归类：
	// 入口未产生错误时，回落到下游 5xx 的归类码（50202），
	// 这样「透传了下游错误」与「入口一切正常」在日志里可以区分。
	entry.Code = httpx.ErrorCode(c)
	entry.ErrorCode = entry.Code
	if entry.ErrorCode == 0 {
		entry.ErrorCode = httpx.Int(c, contracts.CtxDownstreamErrorCode)
	}

	if entry.Route == "" {
		entry.Route = contracts.FamilyUnknown
	}
	return entry
}

// responseSize 返回响应体大小。
//
// 关键约束：绝不能调用 Response.Body() 去量长度——转发类响应是流式的，
// 读一次就会把流读干，Hertz 最终只会写出空响应体；同时大文件会被整体缓冲进内存。
// 因此流式响应只读取已声明的 Content-Length，未知长度记 -1。
func responseSize(c *app.RequestContext) int {
	if c.Response.IsBodyStream() {
		if length := c.Response.Header.ContentLength(); length >= 0 {
			return length
		}
		return -1
	}
	return len(c.Response.Body())
}
