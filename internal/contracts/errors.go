// Package contracts 定义入口层对外与对内的稳定契约：错误码、错误响应体、
// 请求头名称、上下文键与路由家族。
//
// 该包不允许依赖任何 HTTP 框架、配置或业务包——契约必须能被任意实现引用，
// 这样后续替换框架或将某类通道改成 gRPC 时，调用方与测试都不需要改动。
package contracts

import "net/http"

// Code 是入口层对外的业务错误码。
// 一个码值同时决定 HTTP 状态码与默认提示文案，避免各处理点各自决定语义。
type Code int

// 错误码表。与《统一入口 P0-P1 技术设计》§5.2 保持一致。
const (
	// CodeOK 表示成功，仅用于内部判断，不会出现在错误响应中。
	CodeOK Code = 0

	CodeBadRequest   Code = 40001 // 参数错误
	CodeBodyTooLarge Code = 40002 // 请求体过大
	CodeUnauthorized Code = 40101 // 未认证
	CodeTokenExpired Code = 40102 // Token 过期
	// CodeScopeInsufficient 表示 Token 有效但缺少该路由所需 scope。
	CodeScopeInsufficient Code = 40103
	CodeForbidden         Code = 40301 // RBAC 拒绝
	CodeRouteNotFound     Code = 40401 // 路由不存在
	CodeAppNotFound       Code = 40402 // 应用未注册
	CodeRateLimited       Code = 42901 // 限流
	CodeInternal          Code = 50001 // 入口内部错误
	// CodeNotImplemented 表示路由已注册但处理器尚未接入下游，属脚手架阶段专用。
	CodeNotImplemented    Code = 50101
	CodeDownstreamRefused Code = 50201 // 下游不可用（拨号失败/连接被拒）
	CodeDownstreamBad     Code = 50202 // 下游响应异常
	CodeDownstreamTimeout Code = 50401 // 下游超时
)

var httpStatusByCode = map[Code]int{
	CodeOK:                http.StatusOK,
	CodeBadRequest:        http.StatusBadRequest,
	CodeBodyTooLarge:      http.StatusRequestEntityTooLarge,
	CodeUnauthorized:      http.StatusUnauthorized,
	CodeTokenExpired:      http.StatusUnauthorized,
	CodeScopeInsufficient: http.StatusForbidden,
	CodeForbidden:         http.StatusForbidden,
	CodeRouteNotFound:     http.StatusNotFound,
	CodeAppNotFound:       http.StatusNotFound,
	CodeRateLimited:       http.StatusTooManyRequests,
	CodeInternal:          http.StatusInternalServerError,
	CodeNotImplemented:    http.StatusNotImplemented,
	CodeDownstreamRefused: http.StatusServiceUnavailable,
	CodeDownstreamBad:     http.StatusBadGateway,
	CodeDownstreamTimeout: http.StatusGatewayTimeout,
}

var defaultMessageByCode = map[Code]string{
	CodeOK:                "ok",
	CodeBadRequest:        "bad request",
	CodeBodyTooLarge:      "request body too large",
	CodeUnauthorized:      "unauthorized",
	CodeTokenExpired:      "token expired",
	CodeScopeInsufficient: "insufficient scope",
	CodeForbidden:         "forbidden",
	CodeRouteNotFound:     "route not found",
	CodeAppNotFound:       "application not registered",
	CodeRateLimited:       "rate limited",
	CodeInternal:          "internal error",
	CodeNotImplemented:    "not implemented",
	CodeDownstreamRefused: "downstream unavailable",
	CodeDownstreamBad:     "downstream bad response",
	CodeDownstreamTimeout: "downstream timeout",
}

// HTTPStatus 返回该错误码对应的 HTTP 状态码；未登记的码值一律按 500 处理，
// 避免出现「200 带错误码」这类前端难以判断的响应。
func (c Code) HTTPStatus() int {
	if status, ok := httpStatusByCode[c]; ok {
		return status
	}
	return http.StatusInternalServerError
}

// DefaultMessage 返回该错误码的默认提示文案。
// 文案面向日志与排障，不直接作为前端展示文案。
func (c Code) DefaultMessage() string {
	if msg, ok := defaultMessageByCode[c]; ok {
		return msg
	}
	return "unknown error"
}

// Known 报告该码值是否已登记，供测试与自检使用。
func (c Code) Known() bool {
	_, ok := httpStatusByCode[c]
	return ok
}
