package observability

import (
	"context"
	"strings"
	"testing"
)

func TestParseTraceParentValid(t *testing.T) {
	raw := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	trace, ok := ParseTraceParent(raw)
	if !ok {
		t.Fatalf("应能解析合法 traceparent: %s", raw)
	}
	if trace.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("TraceID = %q", trace.TraceID)
	}
	if trace.SpanID != "00f067aa0ba902b7" {
		t.Errorf("SpanID = %q", trace.SpanID)
	}
	if trace.Header() != raw {
		t.Errorf("重新组装后不一致: %q", trace.Header())
	}
	if !trace.Valid() {
		t.Error("合法上下文应 Valid")
	}
}

func TestParseTraceParentInvalid(t *testing.T) {
	cases := map[string]string{
		"空值":            "",
		"段数不足":          "00-4bf92f3577b34da6a3ce929d0e0e4736-01",
		"版本不支持":         "01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"trace 长度不足":    "00-4bf92f-00f067aa0ba902b7-01",
		"trace 含大写":     "00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01",
		"trace 非十六进制":   "00-zzz92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"trace 全零":      "00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"span 全零":       "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",
		"flags 长度非法":    "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-1",
		"flags 非十六进制字符": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-zz",
	}

	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := ParseTraceParent(raw); ok {
				t.Errorf("非法 traceparent 不应解析成功: %q", raw)
			}
		})
	}
}

func TestNewTraceContextIsValidAndUnique(t *testing.T) {
	first := NewTraceContext()
	second := NewTraceContext()

	if !first.Valid() {
		t.Fatalf("新生成的上下文应有效: %+v", first)
	}
	if first.TraceID == second.TraceID {
		t.Error("两次生成的 trace-id 不应相同")
	}
	if _, ok := ParseTraceParent(first.Header()); !ok {
		t.Errorf("自己生成的头必须能再次解析: %q", first.Header())
	}
}

func TestNewRequestIDFormat(t *testing.T) {
	seen := make(map[string]struct{}, 128)
	for i := 0; i < 128; i++ {
		id := NewRequestID()
		if len(id) != requestIDLen*2 {
			t.Fatalf("请求标识长度应为 %d，实际 %d", requestIDLen*2, len(id))
		}
		if strings.ToLower(id) != id {
			t.Fatalf("请求标识应为小写十六进制: %q", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("请求标识出现重复: %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestTraceContextHeaderDefaults(t *testing.T) {
	trace := TraceContext{TraceID: NewTraceID(), SpanID: newHex(spanIDBytes)}
	header := trace.Header()
	parts := strings.Split(header, "-")
	if len(parts) != 4 {
		t.Fatalf("头格式异常: %q", header)
	}
	if parts[0] != traceParentVersion || parts[3] != TraceFlagsSampled {
		t.Errorf("默认版本/标志位异常: %q", header)
	}
}

func TestContextHelpers(t *testing.T) {
	ctx := context.Background()
	if RequestIDFrom(ctx) != "" || TraceIDFrom(ctx) != "" {
		t.Error("空上下文应返回空串")
	}

	ctx = WithRequestID(ctx, "req-1")
	ctx = WithTraceID(ctx, "trace-1")
	if RequestIDFrom(ctx) != "req-1" {
		t.Errorf("RequestIDFrom = %q", RequestIDFrom(ctx))
	}
	if TraceIDFrom(ctx) != "trace-1" {
		t.Errorf("TraceIDFrom = %q", TraceIDFrom(ctx))
	}

	// nil 上下文不得 panic。
	var nilCtx context.Context
	if RequestIDFrom(nilCtx) != "" || TraceIDFrom(nilCtx) != "" {
		t.Error("nil 上下文应返回空串")
	}
}
