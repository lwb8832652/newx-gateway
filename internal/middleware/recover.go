// Package middleware 提供入口的全局与家族级中间件。
//
// 中间件顺序在 internal/app 中显式固定，顺序语义如下：
//
//	Recover → Tracing → RequestLogger → HeaderGuard → BodyLimit → 家族中间件 → 处理器
//
// 顺序不可随意调整：
//   - Recover 必须最外层，才能兜住后续所有 panic；
//   - Tracing 必须先于日志，否则日志拿不到 request_id/trace_id；
//   - HeaderGuard 必须早于任何读取身份的中间件，先剥离伪造头再注入可信值。
package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"

	"github.com/cloudwego/hertz/pkg/app"

	"newx-gateway/internal/contracts"
	"newx-gateway/internal/httpx"
)

// Recover 捕获处理器 panic，记录堆栈并返回统一内部错误，保证连接不被中断。
func Recover(logger *slog.Logger) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}

			message := "internal error"
			if logger != nil {
				logger.Error("panic recovered",
					"request_id", httpx.RequestID(c),
					"trace_id", httpx.TraceID(c),
					"app_code", httpx.AppCode(c),
					"route", httpx.Route(c),
					"method", string(c.Method()),
					"panic_type", fmt.Sprintf("%T", recovered),
					"stack", string(debug.Stack()),
				)
			}

			// 响应已开始写出时不能再改写状态码与响应体，否则会污染已发送的数据，
			// 此时只登记错误码，保证日志仍能反映失败原因。
			if responseStarted(c) {
				httpx.MarkError(c, contracts.CodeInternal, message)
				return
			}
			httpx.WriteErrorWithMessage(c, contracts.CodeInternal, message)
		}()

		c.Next(ctx)
	}
}

// responseStarted 判断响应是否已经写出。Hertz 默认状态码为 200，
// 因此「非 200 状态 或 已有响应体」视为已开始写出。
func responseStarted(c *app.RequestContext) bool {
	if c == nil {
		return false
	}
	if c.Response.StatusCode() != 200 {
		return true
	}
	return len(c.Response.Body()) > 0
}
