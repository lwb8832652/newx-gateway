package contracts

// ErrorResponse 是入口自身产生的错误响应体（设计文档 §5.1）。
//
// 成功响应由下游原样透传，入口不做包裹——避免破坏既有前端解析。
//
// 字段兼容说明：yudao 的统一响应使用 {code, msg, data}，既有前端普遍读 msg。
// 入口同时输出 message（本工程契约）与 msg（yudao 兼容字段），二者内容一致；
// 只增加字段、不改变含义，因此对已有调用方保持向后兼容。
type ErrorResponse struct {
	Code      int    `json:"code"`
	Msg       string `json:"msg"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Data      any    `json:"data"`
}

// NewError 用错误码的默认文案构造错误响应。
func NewError(code Code, requestID string) ErrorResponse {
	return NewErrorWithMessage(code, requestID, code.DefaultMessage())
}

// NewErrorWithMessage 用自定义文案构造错误响应，文案仅用于补充排障信息，
// 不得携带堆栈、SQL、下游内部地址等敏感内容。
func NewErrorWithMessage(code Code, requestID, message string) ErrorResponse {
	return ErrorResponse{
		Code:      int(code),
		Msg:       message,
		Message:   message,
		RequestID: requestID,
		Data:      nil,
	}
}
