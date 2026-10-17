package contracts

// ErrorResponse 是入口自身产生的错误响应体（设计文档 §5.1）。
//
// 成功响应由下游原样透传，入口不做包裹——避免破坏既有前端解析。
type ErrorResponse struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Data      any    `json:"data"`
}

// NewError 用错误码的默认文案构造错误响应。
func NewError(code Code, requestID string) ErrorResponse {
	return ErrorResponse{
		Code:      int(code),
		Message:   code.DefaultMessage(),
		RequestID: requestID,
		Data:      nil,
	}
}

// NewErrorWithMessage 用自定义文案构造错误响应，文案仅用于补充排障信息，
// 不得携带堆栈、SQL、下游内部地址等敏感内容。
func NewErrorWithMessage(code Code, requestID, message string) ErrorResponse {
	return ErrorResponse{
		Code:      int(code),
		Message:   message,
		RequestID: requestID,
		Data:      nil,
	}
}
