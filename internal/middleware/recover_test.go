package middleware

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"

	"newx-gateway/internal/contracts"
)

func TestRecoverDoesNotExposePanicValue(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-response", true: "after-response"}[started], func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			c := app.NewContext(0)
			c.Request.Header.SetMethod(http.MethodPost)
			c.Request.SetRequestURI("/web/huobao/v1/tasks?token=private-query")
			if started {
				c.Response.SetBodyString("original response")
			}
			c.SetHandlers(app.HandlersChain{
				Recover(logger),
				func(context.Context, *app.RequestContext) { panic("private-panic-body") },
			})
			c.Next(context.Background())
			if started {
				if string(c.Response.Body()) != "original response" {
					t.Error("响应已开始后不得追加错误体")
				}
			} else if c.Response.StatusCode() != http.StatusInternalServerError {
				t.Errorf("状态码 = %d，期望 500", c.Response.StatusCode())
			}
			for _, secret := range []string{"private-panic-body", "private-query"} {
				if strings.Contains(logs.String(), secret) || strings.Contains(string(c.Response.Body()), secret) {
					t.Errorf("panic 内容泄露: %q", secret)
				}
				if value, ok := c.Get(contracts.CtxErrMsg); ok && strings.Contains(value.(string), secret) {
					t.Error("上下文错误不应保存 panic 原文")
				}
			}
			if !strings.Contains(logs.String(), "panic_type") || !strings.Contains(logs.String(), "stack") {
				t.Error("异常日志应保留类型和调用栈用于排障")
			}
		})
	}
}
