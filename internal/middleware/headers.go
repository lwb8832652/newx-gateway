package middleware

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"newx-gateway/internal/contracts"
)

// HeaderGuard 剥离客户端伪造的内部身份头。
//
// 攻击面：客户端直接向后端注入 X-User-Id / X-App-Code 等头，冒充他人身份。
// 防御：入口是唯一可信注入点，所有同名头在进入后续中间件前一律删除，
// 再由入口按自身解析结果重新注入。遍历与删除必须分两步，否则会漏删。
func HeaderGuard() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		var spoofed []string
		c.Request.Header.VisitAll(func(key, _ []byte) {
			if contracts.IsSpoofableHeader(string(key)) {
				spoofed = append(spoofed, string(key))
			}
		})
		for _, key := range spoofed {
			c.Request.Header.Del(key)
		}

		c.Next(ctx)
	}
}
