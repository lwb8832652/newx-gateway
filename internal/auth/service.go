package auth

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"

	"newx-gateway/internal/contracts"
	"newx-gateway/internal/httpx"
)

// bearerPrefix 是 Authorization 头的方案前缀。
const bearerPrefix = "Bearer "

// Options 是认证中间件配置。
type Options struct {
	// Enabled 为 false 时中间件直接放行（不解析身份）。
	Enabled bool
	// CacheTTL 是登录用户缓存时长，默认 1 分钟（与 yudao 一致）。
	CacheTTL time.Duration
}

// Service 是认证中间件。
type Service struct {
	client *TokenClient
	cache  *loginUserCache
	opts   Options
	logger *slog.Logger
}

// New 构建认证服务。client 为空时中间件退化为放行。
func New(client *TokenClient, opts Options, logger *slog.Logger) *Service {
	if !opts.Enabled || client == nil {
		return &Service{opts: Options{Enabled: false}, logger: logger}
	}
	return &Service{
		client: client,
		cache:  newLoginUserCache(opts.CacheTTL),
		opts:   opts,
		logger: logger,
	}
}

// Enabled 报告认证是否生效。
func (s *Service) Enabled() bool { return s != nil && s.opts.Enabled }

// Middleware 返回认证中间件。
//
// 处理顺序与 yudao 的 TokenAuthenticationFilter 一致：
//  1. 剥离客户端伪造的 login-user，校验 tenant-id 提示格式（由 HeaderGuard 完成）；
//  2. 无 Bearer Token → 直接放行；
//  3. 有 Token → 查缓存，未命中调 check-token；
//  4. 解析出未过期用户 → 注入 login-user / tenant-id 头与上下文；
//  5. 解析失败或已过期 → 放行但不带身份，由下游判断是否需要登录。
func (s *Service) Middleware() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		if !s.Enabled() {
			c.Next(ctx)
			return
		}

		accessToken := ObtainAuthorization(c)
		if accessToken == "" {
			// 无令牌：不放任何身份头，直接继续。
			c.Next(ctx)
			return
		}

		// 租户编号从客户端请求头读取（与 yudao 行为一致），仅作为缓存键与
		// 校验请求的租户提示；真正的租户归属以 check-token 返回结果为准。
		tenantID := tenantIDFromRequest(c)

		user, err := s.cache.Do(ctx, cacheKey(tenantID, accessToken), func(loadCtx context.Context) (*LoginUser, error) {
			return s.client.Check(loadCtx, tenantID, accessToken)
		})
		if err != nil {
			// 鉴权方不可用：按无身份放行，避免入口成为单点。
			// 这与 yudao 的降级方向一致——它同样不会因为校验失败而拒绝请求。
			if s.logger != nil {
				s.logger.Warn("校验访问令牌失败，按无身份放行",
					"request_id", httpx.RequestID(c),
					"error_type", fmt.Sprintf("%T", err),
				)
			}
			c.Next(ctx)
			return
		}
		if user.Empty() || user.Expired(time.Now()) {
			c.Next(ctx)
			return
		}

		inject(c, user)
		c.Next(ctx)
	}
}

// inject 把登录用户写入上下文与下游请求头。
func inject(c *app.RequestContext, user *LoginUser) {
	headerValue, err := user.HeaderValue()
	if err != nil {
		// 序列化失败时不能静默丢身份：记录后仍放行，让下游按未登录处理。
		return
	}

	c.Set(contracts.CtxPrincipalID, strconv.FormatInt(user.ID, 10))
	c.Request.Header.Set(contracts.HeaderLoginUser, headerValue)
	c.Request.Header.Set(contracts.HeaderTenantID, strconv.FormatInt(user.TenantID, 10))
}

// ObtainAuthorization 从请求头解析 Bearer 令牌。
//
// 与 yudao 的 obtainAuthorization 保持一致：宽松查找 "Bearer " 子串，
// 找不到返回空串。刻意不在这里做强校验——校验是 check-token 的职责。
func ObtainAuthorization(c *app.RequestContext) string {
	authorization := c.Request.Header.Get(contracts.HeaderAuthorization)
	if authorization == "" {
		return ""
	}
	index := strings.Index(authorization, bearerPrefix)
	if index < 0 {
		return ""
	}
	return strings.TrimSpace(authorization[index+len(bearerPrefix):])
}

// tenantIDFromRequest 读取租户提示，缺失或非法返回 -1，与显式租户 0 区分。
func tenantIDFromRequest(c *app.RequestContext) int64 {
	raw := strings.TrimSpace(c.Request.Header.Get(contracts.HeaderTenantID))
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return -1
	}
	return value
}

// CacheStats 返回缓存条目数，供自检接口与测试使用。
func (s *Service) CacheStats() int {
	if s == nil || s.cache == nil {
		return 0
	}
	return s.cache.Len()
}
