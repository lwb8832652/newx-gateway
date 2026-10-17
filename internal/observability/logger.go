// Package observability 负责日志与链路标识两类横切能力。
//
// 该包不依赖 HTTP 框架：日志器与标识工具都能在单测里直接使用。
package observability

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// NewLogger 按配置构造日志器。
// format 支持 json / text；level 支持 debug / info / warn / error。
func NewLogger(level, format string, out io.Writer) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(strings.TrimSpace(level))); err != nil {
		return nil, fmt.Errorf("日志级别非法 %q: %w", level, err)
	}

	options := &slog.HandlerOptions{Level: lvl}
	var handler slog.Handler
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "json", "":
		handler = slog.NewJSONHandler(out, options)
	case "text":
		handler = slog.NewTextHandler(out, options)
	default:
		return nil, fmt.Errorf("日志格式非法 %q（可选 json/text）", format)
	}
	return slog.New(handler), nil
}

// RequestLog 是一条请求日志的固定字段集合，对应技术设计 §11。
//
// 字段顺序与命名不可随意变更：日志是排障与审计的对外契约。
// 禁止在此结构上追加 Token、Authorization、请求/响应正文等敏感字段。
type RequestLog struct {
	RequestID    string
	TraceID      string
	AppCode      string
	Route        string
	Method       string
	Downstream   string
	UpstreamHost string
	Status       int
	Code         int
	LatencyMS    float64
	ReqBytes     int64
	RespBytes    int
	PrincipalID  string
	Retry        int
	ErrorCode    int
	ErrMsg       string
}

// attrs 把日志字段展开为 slog 属性。
func (r RequestLog) attrs() []any {
	attrs := make([]any, 0, 32)
	attrs = append(attrs,
		slog.String("request_id", r.RequestID),
		slog.String("trace_id", r.TraceID),
		slog.String("app_code", r.AppCode),
		slog.String("route", r.Route),
		slog.String("method", r.Method),
		slog.String("downstream", r.Downstream),
		slog.String("upstream_host", r.UpstreamHost),
		slog.Int("status", r.Status),
		slog.Int("code", r.Code),
		slog.Float64("latency_ms", r.LatencyMS),
		slog.Int64("req_bytes", r.ReqBytes),
		slog.Int("resp_bytes", r.RespBytes),
		slog.String("principal_id", r.PrincipalID),
		slog.Int("retry", r.Retry),
		slog.Int("error_code", r.ErrorCode),
	)
	// 仅在确有错误信息时输出，避免正常请求出现空字段噪声。
	if r.ErrMsg != "" {
		attrs = append(attrs, slog.String("err_msg", r.ErrMsg))
	}
	return attrs
}

// Level 依据状态码与错误码推断日志级别：
// 5xx 或存在入口错误码 → ERROR；4xx → WARN；其余 INFO。
func (r RequestLog) Level() slog.Level {
	switch {
	case r.Status >= 500 || r.ErrorCode >= 50000:
		return slog.LevelError
	case r.Status >= 400 || r.ErrorCode != 0:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// Log 输出单行结构化日志。
func (r RequestLog) Log(logger *slog.Logger) {
	if logger == nil {
		return
	}
	logger.Log(nil, r.Level(), "request", r.attrs()...)
}
