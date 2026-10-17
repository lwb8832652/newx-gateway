package observability

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestNewLoggerRejectsInvalidConfig(t *testing.T) {
	if _, err := NewLogger("verbose", "json", &bytes.Buffer{}); err == nil {
		t.Error("非法日志级别应报错")
	}
	if _, err := NewLogger("info", "xml", &bytes.Buffer{}); err == nil {
		t.Error("非法日志格式应报错")
	}
}

func TestNewLoggerLevelFiltering(t *testing.T) {
	buf := &bytes.Buffer{}
	logger, err := NewLogger("warn", "json", buf)
	if err != nil {
		t.Fatalf("构造日志器失败: %v", err)
	}

	logger.Info("should be dropped")
	if buf.Len() != 0 {
		t.Errorf("info 级别应被过滤，实际输出: %s", buf.String())
	}
	logger.Warn("should appear")
	if !strings.Contains(buf.String(), "should appear") {
		t.Errorf("warn 级别应输出，实际: %s", buf.String())
	}
}

func TestRequestLogEmitsFixedFields(t *testing.T) {
	buf := &bytes.Buffer{}
	logger, err := NewLogger("debug", "json", buf)
	if err != nil {
		t.Fatalf("构造日志器失败: %v", err)
	}

	log := RequestLog{
		RequestID:    "req-1",
		TraceID:      "trace-1",
		AppCode:      "huobao",
		Route:        "web",
		Method:       "GET",
		Downstream:   "huobao",
		UpstreamHost: "127.0.0.1:5679",
		Status:       501,
		Code:         50101,
		LatencyMS:    1.25,
		ReqBytes:     10,
		RespBytes:    20,
		PrincipalID:  "",
		Retry:        0,
		ErrorCode:    50101,
		ErrMsg:       "not implemented",
	}
	log.Log(logger)

	var decoded map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("日志必须是单行 JSON: %v，原文 %s", err, buf.String())
	}

	required := []string{
		"time", "level", "msg",
		"request_id", "trace_id", "app_code", "route", "method", "downstream",
		"upstream_host", "status", "code", "latency_ms", "req_bytes", "resp_bytes",
		"principal_id", "retry", "error_code", "err_msg",
	}
	for _, field := range required {
		if _, ok := decoded[field]; !ok {
			t.Errorf("日志缺少字段 %q，实际: %s", field, buf.String())
		}
	}

	if decoded["request_id"] != "req-1" || decoded["app_code"] != "huobao" {
		t.Errorf("字段取值异常: %s", buf.String())
	}
}

func TestRequestLogOmitsEmptyErrMsg(t *testing.T) {
	buf := &bytes.Buffer{}
	logger, err := NewLogger("info", "json", buf)
	if err != nil {
		t.Fatalf("构造日志器失败: %v", err)
	}

	RequestLog{RequestID: "req-1", Status: 200}.Log(logger)
	if strings.Contains(buf.String(), "err_msg") {
		t.Errorf("正常请求不应输出 err_msg: %s", buf.String())
	}
}

func TestRequestLogLevel(t *testing.T) {
	cases := []struct {
		name string
		log  RequestLog
		want slog.Level
	}{
		{"成功", RequestLog{Status: 200}, slog.LevelInfo},
		{"客户端错误", RequestLog{Status: 404, ErrorCode: 40401}, slog.LevelWarn},
		{"服务端错误", RequestLog{Status: 502, ErrorCode: 50201}, slog.LevelError},
		{"仅错误码", RequestLog{Status: 200, ErrorCode: 50201}, slog.LevelError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.log.Level(); got != tc.want {
				t.Errorf("Level() = %v，期望 %v", got, tc.want)
			}
		})
	}
}

func TestRequestLogNilLoggerIsSafe(t *testing.T) {
	RequestLog{RequestID: "req-1"}.Log(nil)
}
