package middleware

import (
	"context"
	"strconv"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"

	"newx-gateway/internal/contracts"
)

// HeaderGuard 剥离客户端伪造的内部身份头。
//
// 攻击面：客户端直接向后端注入 X-User-Id / X-App-Code 等头，冒充他人身份。
// 防御：入口是唯一可信注入点，所有同名头在进入后续中间件前一律删除，
// 再由入口按自身解析结果重新注入。遍历与删除必须分两步，否则会漏删。
//
// trustProxy 为 true 时（入口前方确有可信代理，如 nginx），保留
// X-Forwarded-For / X-Forwarded-Proto / X-Forwarded-Host / X-Real-Ip，
// 否则下游只能看到「直连入口的那一跳」，真实客户端 IP 会丢失在审计日志里。
// 该开关同时决定转发层是否追加 X-Forwarded-For，两处语义必须一致。
func HeaderGuard(trustProxy bool) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		// tenant-id 是客户端可提供的租户选择提示，不是可信登录身份。
		// 规范化后供匿名接口与 check-token 使用；认证成功时由真实租户覆盖。
		rawTenant := strings.TrimSpace(c.Request.Header.Get(contracts.HeaderTenantID))
		tenantID, err := strconv.ParseInt(rawTenant, 10, 64)
		c.Request.Header.Del(contracts.HeaderTenantID)
		if err == nil && tenantID >= 0 {
			c.Request.Header.Set(contracts.HeaderTenantID, strconv.FormatInt(tenantID, 10))
		}
		var spoofed []string
		c.Request.Header.VisitAll(func(key, _ []byte) {
			name := string(key)
			if !contracts.IsSpoofableHeader(name) {
				return
			}
			// 可信代理场景下，来源标识头由上游代理写入，必须继续透传，
			// 否则下游与审计日志拿不到真实客户端地址。
			if trustProxy && contracts.IsForwardedHeader(name) {
				return
			}
			spoofed = append(spoofed, name)
		})
		for _, key := range spoofed {
			c.Request.Header.Del(key)
		}

		c.Next(ctx)
	}
}
