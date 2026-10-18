package contracts

import "strings"

// 入口与下游之间约定的请求头。
// X-Request-Id / traceparent 可由客户端携带；tenant-id 可作为未认证的租户提示，
// 不代表登录身份。登录成功后 tenant-id 以 check-token 返回的租户覆盖。
const (
	// HeaderRequestID 请求标识，入口保证每条请求都有值。
	HeaderRequestID = "X-Request-Id"
	// HeaderTraceID 链路标识（W3C trace-id 的独立副本，便于日志直读）。
	HeaderTraceID = "X-Trace-Id"
	// HeaderTraceParent W3C 链路上下文，原样透传。
	HeaderTraceParent = "traceparent"
	// HeaderAppCode 入口解析出的应用编码。
	HeaderAppCode = "X-App-Code"
	// HeaderUserID 入口校验身份后注入的用户标识。
	HeaderUserID = "X-User-Id"
	// HeaderUserScope 入口注入的授权范围，逗号分隔。
	HeaderUserScope = "X-User-Scope"
	// HeaderPrincipalID 入口注入的主体标识，用于限流与审计归类。
	HeaderPrincipalID = "X-Principal-Id"
	// HeaderLoginUser 是 yudao 框架约定的登录用户头：
	// LoginUser 对象序列化为 JSON 后再 URL-encode。这是芋道私有约定，
	// 不是 OAuth2/OIDC 标准，字段与编码方式必须与 yudao-spring-boot-starter-security 完全一致。
	HeaderLoginUser = "login-user"
	// HeaderTenantID 是 yudao 约定的租户头。
	HeaderTenantID = "tenant-id"
)

// 代理与鉴权相关头。这些头客户端可以伪造，入口默认不信任。
const (
	HeaderAuthorization  = "Authorization"
	HeaderCookie         = "Cookie"
	HeaderSetCookie      = "Set-Cookie"
	HeaderForwardedFor   = "X-Forwarded-For"
	HeaderForwardedHost  = "X-Forwarded-Host"
	HeaderForwardedProto = "X-Forwarded-Proto"
	HeaderRealIP         = "X-Real-Ip"
	HeaderRetryAfter     = "Retry-After"
)

// internalHeaderPrefix 覆盖所有 X-Internal-* 形式的内网专用头。
const internalHeaderPrefix = "x-internal-"

// SpoofableHeaders 是入口必须无条件剥离的客户端伪造头。
// 剥离发生在注入之前，确保下游与日志看到的身份信息只来自入口自身。
//
// login-user 必须剥离，避免伪造管理员身份；tenant-id 仅为租户提示，
// 由 HeaderGuard 校验格式，不能作为已经认证的租户身份使用。
var SpoofableHeaders = []string{
	HeaderUserID,
	HeaderUserScope,
	HeaderPrincipalID,
	HeaderAppCode,
	HeaderLoginUser,
	HeaderForwardedFor,
	HeaderForwardedHost,
	HeaderForwardedProto,
	HeaderRealIP,
}

// IsSpoofableHeader 判断某个请求头是否必须被剥离。
// 比较大小写不敏感，并且拦截全部 X-Internal-* 前缀头。
func IsSpoofableHeader(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, internalHeaderPrefix) {
		return true
	}
	for _, candidate := range SpoofableHeaders {
		if lower == strings.ToLower(candidate) {
			return true
		}
	}
	return false
}

// forwardedHeaders 是在可信代理场景下允许继续透传的来源标识头。
// 只有 TRUST_PROXY_HEADERS=true（入口前方确有可信代理）时才放行。
var forwardedHeaders = map[string]bool{
	strings.ToLower(HeaderForwardedFor):   true,
	strings.ToLower(HeaderForwardedHost):  true,
	strings.ToLower(HeaderForwardedProto): true,
	strings.ToLower(HeaderRealIP):         true,
}

// IsForwardedHeader 判断是否为来源标识头（X-Forwarded-* / X-Real-Ip）。
func IsForwardedHeader(name string) bool { return forwardedHeaders[strings.ToLower(name)] }

// IsSensitiveHeader 判断某个请求头是否禁止写入日志。
func IsSensitiveHeader(name string) bool {
	switch strings.ToLower(name) {
	case strings.ToLower(HeaderAuthorization), strings.ToLower(HeaderCookie), strings.ToLower(HeaderSetCookie):
		return true
	default:
		return false
	}
}
