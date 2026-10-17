package contracts

// 请求级上下文键。统一放在契约包，避免各中间件与处理器各自定义同名键。
// 这些键同时用于 Hertz 的 RequestContext.Set/Get 与标准 context.Context。
const (
	CtxRequestID  = "newx.request_id"
	CtxTraceID    = "newx.trace_id"
	CtxAppCode    = "newx.app_code"
	CtxRoute      = "newx.route"
	CtxDownstream = "newx.downstream"
	// CtxUpstreamHost 记录下游主机名，仅用于日志，不参与路由。
	CtxUpstreamHost = "newx.upstream_host"
	// CtxPrincipalID 记录通过鉴权的主体标识，本阶段为空。
	CtxPrincipalID = "newx.principal_id"
	// CtxUserScope 记录通过鉴权后解析出的授权范围，本阶段为空。
	CtxUserScope = "newx.user_scope"
	// CtxErrorCode 记录入口已经写出的业务错误码，供日志字段 code/error_code 使用。
	CtxErrorCode = "newx.error_code"
	// CtxErrMsg 记录入口写出的错误说明，供日志 err_msg 使用。
	CtxErrMsg = "newx.err_msg"
	// CtxRateBucket 记录本次请求命中的限流分桶，供日志与排障使用。
	CtxRateBucket = "newx.rate_bucket"
)

// 路由家族标识。与 URL 前缀一一对应，供限流分桶、日志归类与后续按家族鉴权使用。
const (
	FamilyWeb     = "web"
	FamilyAdmin   = "admin"
	FamilyMCP     = "mcp"
	FamilyOpenAPI = "openapi"
	FamilyHealth  = "health"
	FamilyUnknown = "unknown"
)

// 路由前缀（不含应用编码与版本号部分）。
const (
	PrefixWeb     = "/web"
	PrefixAdmin   = "/admin"
	PrefixMCP     = "/mcp"
	PrefixOpenAPI = "/open-api"
	PrefixStatic  = "/static"
	PrefixHealthz = "/healthz"
	PrefixReadyz  = "/readyz"
)
