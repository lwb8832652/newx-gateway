package contracts

import "strings"

// 入口与下游之间约定的请求头。
// 除 X-Request-Id / traceparent 由客户端携带时透传外，其余均由入口注入。
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
var SpoofableHeaders = []string{
	HeaderUserID,
	HeaderUserScope,
	HeaderPrincipalID,
	HeaderAppCode,
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

// IsSensitiveHeader 判断某个请求头是否禁止写入日志。
func IsSensitiveHeader(name string) bool {
	switch strings.ToLower(name) {
	case strings.ToLower(HeaderAuthorization), strings.ToLower(HeaderCookie), strings.ToLower(HeaderSetCookie):
		return true
	default:
		return false
	}
}
