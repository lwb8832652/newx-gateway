package transport

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"newx-gateway/internal/contracts"
	"newx-gateway/internal/httpx"
)

// NoRouteHandler 是兜底处理器：未命中任何已注册路由的请求统一返回「路由不存在」。
//
// 关闭的通道（如 OpenAPI 默认关闭）不注册路由，其路径同样落到这里——
// 请求方无法通过响应差异判断某个通道是否存在，减少探测面。
func NoRouteHandler() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		httpx.SetRoute(c, contracts.FamilyUnknown)
		httpx.WriteError(c, contracts.CodeRouteNotFound)
	}
}
