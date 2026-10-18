package middleware

import (
	"context"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
)

// CORS 对齐 yudao-gateway 的 CorsFilter 行为：
//   - 带 Origin 的请求补齐跨域响应头，预检请求在入口直接返回，不落下游；
//   - 已由下游写出的 CORS 头不重复添加（对齐 yudao 的 DedupeResponseHeader），
//     避免 Access-Control-Allow-Origin 出现两个值被浏览器判定为失败；
//   - 回显 Origin 而不是写死 *，因为 yudao 的跨域配置允许携带 Cookie
//     （Access-Control-Allow-Credentials=true），而 * 与凭证模式互斥。
//
// 安全边界：本中间件与 yudao 网关一致，默认信任任意来源，不承担 CSRF 防护；
// 真正的身份校验由下游 RBAC 负责，浏览器侧凭证策略由 Cookie 的 SameSite 控制。
func CORS() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		origin := strings.TrimSpace(string(c.Request.Header.Peek("Origin")))
		if origin == "" {
			// 非跨域请求原样放行，不添加任何 CORS 头。
			c.Next(ctx)
			return
		}

		// 预检请求：浏览器只关心响应头，入口直接结束，避免占用下游连接。
		if string(c.Method()) == "OPTIONS" &&
			len(c.Request.Header.Peek("Access-Control-Request-Method")) > 0 {
			writePreflight(c, origin)
			return
		}

		c.Next(ctx)

		// 下游（yudao 服务自带 CorsFilter）已经给出跨域头时原样沿用，
		// 只补缺失的部分，保证响应里每个头最多一个值。
		if len(c.Response.Header.Peek("Access-Control-Allow-Origin")) > 0 {
			return
		}
		writeAllowOrigin(c, origin)
		c.Response.Header.Set("Access-Control-Allow-Credentials", "true")
	}
}

// writePreflight 写出预检响应：允许的方法与请求头直接回显浏览器声明，
// 避免维护第二份白名单；MAX_AGE 与 yudao 网关保持一致。
func writePreflight(c *app.RequestContext, origin string) {
	writeAllowOrigin(c, origin)

	method := strings.TrimSpace(string(c.Request.Header.Peek("Access-Control-Request-Method")))
	if method == "" {
		method = "*"
	}
	headers := strings.TrimSpace(string(c.Request.Header.Peek("Access-Control-Request-Headers")))
	if headers == "" {
		headers = "*"
	}

	c.Response.Header.Set("Access-Control-Allow-Methods", method)
	c.Response.Header.Set("Access-Control-Allow-Headers", headers)
	c.Response.Header.Set("Access-Control-Allow-Credentials", "true")
	c.Response.Header.Set("Access-Control-Max-Age", "3600")
	c.Response.Header.Add("Vary", "Access-Control-Request-Method")
	c.Response.Header.Add("Vary", "Access-Control-Request-Headers")

	c.Response.SetStatusCode(200)
	c.Response.SetBody(nil)
	c.Abort()
}

// writeAllowOrigin 回显来源并声明 Vary，保证多来源缓存不会串味。
func writeAllowOrigin(c *app.RequestContext, origin string) {
	c.Response.Header.Set("Access-Control-Allow-Origin", origin)
	c.Response.Header.Add("Vary", "Origin")
}
