package middleware

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"newx-gateway/internal/contracts"
	"newx-gateway/internal/httpx"
)

// BodyLimit 按 Content-Length 提前拒绝超大请求体。
//
// 与 Hertz 的 WithMaxRequestBodySize 是两道防线，职责不同：
//   - Hertz 选项在协议层拦截，避免读入超大 body，是兜底；
//   - 本中间件给出统一的 40002 错误码，是面向客户端的语义层。
//
// 分块传输（Content-Length 为 -1）无法提前判断，交由协议层兜底。
func BodyLimit(maxBytes int64) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		if maxBytes > 0 {
			if length := c.Request.Header.ContentLength(); length > 0 && int64(length) > maxBytes {
				httpx.WriteError(c, contracts.CodeBodyTooLarge)
				return
			}
		}
		c.Next(ctx)
	}
}
