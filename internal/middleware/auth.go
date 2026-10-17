package middleware

import (
	"context"
	"log/slog"

	"github.com/cloudwego/hertz/pkg/app"

	"newx-gateway/internal/contracts"
	"newx-gateway/internal/httpx"
	"newx-gateway/internal/observability"
)

// Authenticator 是鉴权中间件的空位实现。
//
// 本阶段职责有限：确保本次请求已有请求标识（Tracing 已保证，这里做防御性兜底），
// 使下游与日志链路完整。**不做任何身份判定**，因为签发方（authstub / Admin Service）
// 尚未接入，此时引入鉴权只会让所有请求失败。
//
// 后续替换点（接入签发方时）：
//  1. 读取 Authorization Bearer Token；
//  2. 用公钥或 JWKS 本地校验签名、iss、aud、exp，过期返回 40102，无效返回 40101；
//  3. 解析 sub/app_code/scope，写入 contracts.CtxPrincipalID 与 contracts.CtxUserScope；
//  4. 注入 X-User-Id / X-User-Scope / X-Principal-Id，供下游做资源归属判断；
//  5. 校验失败立即返回，不继续调用下游。
func Authenticator(logger *slog.Logger) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		if httpx.RequestID(c) == "" {
			requestID := observability.NewRequestID()
			c.Set(contracts.CtxRequestID, requestID)
			c.Request.Header.Set(contracts.HeaderRequestID, requestID)
			c.Response.Header.Set(contracts.HeaderRequestID, requestID)
			ctx = observability.WithRequestID(ctx, requestID)
			if logger != nil {
				logger.Debug("请求缺少标识，已由鉴权中间件补齐", "route", httpx.Route(c))
			}
		}
		c.Next(ctx)
	}
}
