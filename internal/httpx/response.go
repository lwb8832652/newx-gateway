package httpx

import (
	"github.com/cloudwego/hertz/pkg/app"

	"newx-gateway/internal/contracts"
)

// WriteError 按错误码写出统一错误响应，并记录错误码供请求日志使用。
//
// 统一入口是唯一错误响应出口：任何处理器都不得自行拼装错误 JSON，
// 否则前端会遇到多种响应形状。
func WriteError(c *app.RequestContext, code contracts.Code) {
	WriteErrorWithMessage(c, code, code.DefaultMessage())
}

// WriteErrorWithMessage 用自定义文案写出统一错误响应。
// 文案仅用于补充排障信息，不得携带堆栈、下游内部地址等敏感内容。
func WriteErrorWithMessage(c *app.RequestContext, code contracts.Code, message string) {
	if c == nil {
		return
	}
	if message == "" {
		message = code.DefaultMessage()
	}

	requestID := RequestID(c)
	c.Set(contracts.CtxErrorCode, int(code))
	c.Set(contracts.CtxErrMsg, message)

	// 响应头回带请求标识，便于前端把界面报错与后端日志对照。
	if requestID != "" {
		c.Response.Header.Set(contracts.HeaderRequestID, requestID)
	}
	c.JSON(code.HTTPStatus(), contracts.NewErrorWithMessage(code, requestID, message))

	// 错误响应是链路终点：必须中断后续处理器，否则会叠加第二次响应体
	// （Hertz 的 Abort 只影响后续处理器，不会清除已写出的状态码与响应体）。
	c.Abort()
}

// WriteJSON 写出成功响应。本阶段仅健康检查与自检接口使用；
// 接入反代后业务响应由下游原样透传，不经过这里。
func WriteJSON(c *app.RequestContext, status int, payload any) {
	if c == nil {
		return
	}
	if requestID := RequestID(c); requestID != "" {
		c.Response.Header.Set(contracts.HeaderRequestID, requestID)
	}
	c.JSON(status, payload)
}

// MarkError 只登记错误码而不改写响应体，供已经写出响应体的场景（如 panic 恢复）使用。
func MarkError(c *app.RequestContext, code contracts.Code, message string) {
	if c == nil {
		return
	}
	c.Set(contracts.CtxErrorCode, int(code))
	c.Set(contracts.CtxErrMsg, message)
}
