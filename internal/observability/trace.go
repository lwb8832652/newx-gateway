package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// W3C traceparent 相关常量。
const (
	traceParentVersion = "00"
	// TraceFlagsSampled 表示该链路被采样，入口不参与采样决策，默认沿用下游可记录的语义。
	TraceFlagsSampled = "01"

	traceIDBytes = 16
	spanIDBytes  = 8
	requestIDLen = 16
)

// TraceContext 是 traceparent 的解析结果。
type TraceContext struct {
	Version string
	TraceID string
	SpanID  string
	Flags   string
}

// Header 按 W3C 格式重新组装 traceparent。
func (t TraceContext) Header() string {
	version := t.Version
	if version == "" {
		version = traceParentVersion
	}
	flags := t.Flags
	if flags == "" {
		flags = TraceFlagsSampled
	}
	return strings.Join([]string{version, t.TraceID, t.SpanID, flags}, "-")
}

// Valid 报告该链路上下文是否具备可用的 trace-id 与 span-id。
func (t TraceContext) Valid() bool {
	return len(t.TraceID) == traceIDBytes*2 && len(t.SpanID) == spanIDBytes*2
}

// NewTraceContext 生成一个全新的链路上下文。
func NewTraceContext() TraceContext {
	return TraceContext{
		Version: traceParentVersion,
		TraceID: NewTraceID(),
		SpanID:  newHex(spanIDBytes),
		Flags:   TraceFlagsSampled,
	}
}

// ParseTraceParent 解析 W3C traceparent 头。
// 版本、长度或字符集不合法时返回 false，由调用方改为生成新的链路上下文——
// 入口不因为客户端传了坏头而拒绝请求。
func ParseTraceParent(value string) (TraceContext, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return TraceContext{}, false
	}

	parts := strings.Split(value, "-")
	if len(parts) != 4 {
		return TraceContext{}, false
	}
	trace := TraceContext{Version: parts[0], TraceID: parts[1], SpanID: parts[2], Flags: parts[3]}

	if trace.Version != traceParentVersion {
		return TraceContext{}, false
	}
	if !isLowerHex(trace.TraceID, traceIDBytes*2) || !isLowerHex(trace.SpanID, spanIDBytes*2) {
		return TraceContext{}, false
	}
	// 全 0 的 trace-id/span-id 在 W3C 规范中是非法的。
	if isAllZero(trace.TraceID) || isAllZero(trace.SpanID) {
		return TraceContext{}, false
	}
	if len(trace.Flags) != 2 || !isLowerHex(trace.Flags, 2) {
		return TraceContext{}, false
	}
	return trace, true
}

// NewTraceID 生成 32 位小写十六进制 trace-id。
func NewTraceID() string { return newHex(traceIDBytes) }

// NewRequestID 生成 32 位小写十六进制请求标识。
func NewRequestID() string { return newHex(requestIDLen) }

// newHex 生成 n 字节随机数据的小写十六进制表示。
// 随机源不可用时 panic：请求标识缺失会破坏全链路排障，宁可失败也不静默降级。
func newHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic("newx-gateway: 无法从系统随机源生成标识: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

func isLowerHex(value string, expectLen int) bool {
	if len(value) != expectLen {
		return false
	}
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

func isAllZero(value string) bool {
	for _, r := range value {
		if r != '0' {
			return false
		}
	}
	return true
}

type contextKey string

const (
	contextKeyRequestID contextKey = "request_id"
	contextKeyTraceID   contextKey = "trace_id"
)

// WithRequestID 把请求标识写入标准上下文。
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, contextKeyRequestID, requestID)
}

// RequestIDFrom 从标准上下文读取请求标识，缺失返回空串。
func RequestIDFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(contextKeyRequestID).(string)
	return value
}

// WithTraceID 把链路标识写入标准上下文。
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, contextKeyTraceID, traceID)
}

// TraceIDFrom 从标准上下文读取链路标识，缺失返回空串。
func TraceIDFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(contextKeyTraceID).(string)
	return value
}
