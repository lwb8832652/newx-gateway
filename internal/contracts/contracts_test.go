package contracts

import (
	"net/http"
	"testing"
)

// allCodes 是错误码表的完整清单。新增错误码时必须同步加入本切片，
// 否则下面的完整性用例会失败——这是刻意的：错误码是对外契约，不允许悄悄增加。
func allCodes() []Code {
	return []Code{
		CodeBadRequest,
		CodeBodyTooLarge,
		CodeUnauthorized,
		CodeTokenExpired,
		CodeScopeInsufficient,
		CodeForbidden,
		CodeRouteNotFound,
		CodeAppNotFound,
		CodeRateLimited,
		CodeInternal,
		CodeNotImplemented,
		CodeDownstreamRefused,
		CodeDownstreamBad,
		CodeDownstreamTimeout,
	}
}

func TestEveryCodeIsRegistered(t *testing.T) {
	seen := make(map[Code]bool, len(allCodes()))
	for _, code := range allCodes() {
		if !code.Known() {
			t.Errorf("错误码 %d 未登记状态码或文案", code)
		}
		if seen[code] {
			t.Errorf("错误码 %d 重复", code)
		}
		seen[code] = true

		if code.HTTPStatus() < 400 && code != CodeOK {
			t.Errorf("错误码 %d 的 HTTP 状态码 %d 不属于错误范围", code, code.HTTPStatus())
		}
		if code.DefaultMessage() == "" {
			t.Errorf("错误码 %d 缺少默认文案", code)
		}
	}
}

func TestHTTPStatusMapping(t *testing.T) {
	cases := map[Code]int{
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

	for code, expected := range cases {
		if actual := code.HTTPStatus(); actual != expected {
			t.Errorf("码 %d 的 HTTP 状态码 = %d，期望 %d", code, actual, expected)
		}
	}

	// 未登记的码值不能退化成 200，否则前端会把错误当成功。
	if unknown := Code(99999); unknown.HTTPStatus() != http.StatusInternalServerError {
		t.Errorf("未登记错误码应返回 500，实际 %d", unknown.HTTPStatus())
	}
	if unknown := Code(99999); unknown.Known() {
		t.Error("未登记错误码不应报告 Known")
	}
}

func TestNewErrorResponse(t *testing.T) {
	response := NewError(CodeNotImplemented, "req-1")
	if response.Code != int(CodeNotImplemented) {
		t.Errorf("Code = %d", response.Code)
	}
	if response.RequestID != "req-1" {
		t.Errorf("RequestID = %q", response.RequestID)
	}
	if response.Message != CodeNotImplemented.DefaultMessage() {
		t.Errorf("Message = %q", response.Message)
	}
	if response.Data != nil {
		t.Errorf("Data 应为 null，实际 %v", response.Data)
	}

	custom := NewErrorWithMessage(CodeAppNotFound, "req-2", "app_code 未注册")
	if custom.Message != "app_code 未注册" {
		t.Errorf("自定义文案未生效: %q", custom.Message)
	}
}

func TestIsSpoofableHeader(t *testing.T) {
	cases := map[string]bool{
		"X-User-Id":          true,
		"x-user-id":          true,
		"X-USER-ID":          true,
		"X-User-Scope":       true,
		"X-App-Code":         true,
		"X-Principal-Id":     true,
		"X-Forwarded-For":    true,
		"X-Real-Ip":          true,
		"X-Internal-Trace":   true,
		"x-internal-anything": true,
		// 入口自己生成并注入的头不在剥离清单中，剥离后由入口重新写入。
		"X-Request-Id":   false,
		"X-Trace-Id":     false,
		"traceparent":    false,
		"Authorization":  false,
		"Content-Type":   false,
		"X-Custom-Other": false,
	}

	for name, expected := range cases {
		if actual := IsSpoofableHeader(name); actual != expected {
			t.Errorf("IsSpoofableHeader(%q) = %v，期望 %v", name, actual, expected)
		}
	}
}

func TestIsSensitiveHeader(t *testing.T) {
	for _, name := range []string{"Authorization", "authorization", "Cookie", "Set-Cookie"} {
		if !IsSensitiveHeader(name) {
			t.Errorf("%q 应被判定为敏感头", name)
		}
	}
	for _, name := range []string{"X-Request-Id", "Content-Type"} {
		if IsSensitiveHeader(name) {
			t.Errorf("%q 不应被判定为敏感头", name)
		}
	}
}

func TestContextKeysAreNamespaced(t *testing.T) {
	keys := []string{
		CtxRequestID, CtxTraceID, CtxAppCode, CtxRoute, CtxDownstream,
		CtxUpstreamHost, CtxPrincipalID, CtxUserScope, CtxErrorCode, CtxErrMsg, CtxRateBucket,
	}
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if key == "" {
			t.Error("上下文键不能为空")
		}
		if seen[key] {
			t.Errorf("上下文键重复: %q", key)
		}
		seen[key] = true
	}
}
