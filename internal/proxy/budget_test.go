package proxy

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"newx-gateway/internal/contracts"
)

func testRules() BudgetRules {
	return BudgetRules{
		GeneratePaths: []string{"/agent", "/tasks", "/merge", "/previs", "/task-queue"},
		UploadPaths:   []string{"/upload"},
	}
}

func TestKindOf(t *testing.T) {
	rules := testRules()

	cases := []struct {
		name   string
		method string
		path   string
		want   string
	}{
		{"普通读", "GET", "/api/v1/dramas", KindRead},
		{"HEAD 归读", "HEAD", "/api/v1/dramas", KindRead},
		{"OPTIONS 归读", "OPTIONS", "/api/v1/dramas", KindRead},
		{"普通写", "POST", "/api/v1/dramas", KindWrite},
		{"DELETE 归写", "DELETE", "/api/v1/dramas/1", KindWrite},
		{"生成类路径优先于方法", "GET", "/api/v1/tasks", KindGenerate},
		{"生成类 POST", "POST", "/api/v1/agent/chat", KindGenerate},
		{"上传类路径", "POST", "/api/v1/upload", KindUpload},
		{"上传优先于生成", "POST", "/api/v1/upload/tasks", KindUpload},
		{"路径大小写不敏感", "get", "/API/V1/DRAMAS", KindRead},
		{"编码生成路径", "GET", "/web/huobao/v1/%74asks/create", KindGenerate},
		{"编码生成 HEAD", "HEAD", "/web/huobao/v1/%61gent/run", KindGenerate},
		{"编码上传路径", "OPTIONS", "/web/huobao/v1/%75pload", KindUpload},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := KindOf(tc.method, tc.path, rules); got != tc.want {
				t.Errorf("KindOf(%q, %q) = %q，期望 %q", tc.method, tc.path, got, tc.want)
			}
		})
	}
}

func TestBudgetPrefersFamilyOverride(t *testing.T) {
	proxyClient := New(Options{
		Budgets: map[string]time.Duration{
			"family:mcp":      60 * time.Second,
			KindRead:          15 * time.Second,
			KindGenerate:      900 * time.Second,
			"family:open-api": 30 * time.Second,
		},
		DefaultBudget: 10 * time.Second,
		Rules:         testRules(),
	})

	cases := []struct {
		name   string
		family string
		method string
		path   string
		want   time.Duration
	}{
		{"家族覆盖优先", "mcp", "POST", "/agent/chat", 60 * time.Second},
		{"按请求类型读", "web", "GET", "/dramas", 15 * time.Second},
		{"按请求类型生成", "web", "POST", "/tasks", 900 * time.Second},
		{"未配置回落默认", "admin", "POST", "/users", 10 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := proxyClient.budget(tc.family, tc.method, tc.path); got != tc.want {
				t.Errorf("预算 = %s，期望 %s", got, tc.want)
			}
		})
	}
}

func TestBudgetDefaultsWhenUnset(t *testing.T) {
	proxyClient := New(Options{})
	// 未配置任何预算时也必须给出一个有限值，避免请求永不过期。
	if got := proxyClient.budget("web", "GET", "/dramas"); got != 60*time.Second {
		t.Errorf("默认预算 = %s，期望 60s", got)
	}
}

func TestNewDefaultsDialTimeoutAndPool(t *testing.T) {
	proxyClient := New(Options{})
	if proxyClient.opts.DialTimeout != 2*time.Second {
		t.Errorf("拨号超时默认值 = %s，期望 2s", proxyClient.opts.DialTimeout)
	}
	if proxyClient.opts.MaxIdleConns != 256 || proxyClient.opts.MaxIdleConnsPerHost != 64 {
		t.Errorf("连接池默认值异常: %d/%d", proxyClient.opts.MaxIdleConns, proxyClient.opts.MaxIdleConnsPerHost)
	}
}

func TestTargetURL(t *testing.T) {
	cases := []struct {
		target Target
		want   string
	}{
		{Target{Scheme: "http", Host: "127.0.0.1:8080", Path: "/api/v1/dramas"}, "http://127.0.0.1:8080/api/v1/dramas"},
		{Target{Host: "127.0.0.1:8080", Path: "dramas"}, "http://127.0.0.1:8080/dramas"},
		{Target{Host: "127.0.0.1:8080"}, "http://127.0.0.1:8080/"},
		{Target{Scheme: "https", Host: "api.example.com", Path: "/v1/x", RawQuery: "a=1&b=2"}, "https://api.example.com/v1/x?a=1&b=2"},
		{Target{Host: "127.0.0.1:8080", Path: "/v1/x", RawQuery: "a=1"}, "http://127.0.0.1:8080/v1/x?a=1"},
	}

	for _, tc := range cases {
		if got := tc.target.URL(); got != tc.want {
			t.Errorf("URL() = %q，期望 %q", got, tc.want)
		}
	}
}

func TestAppendForwardedFor(t *testing.T) {
	if got := appendForwardedFor("", "1.2.3.4"); got != "1.2.3.4" {
		t.Errorf("空链路应只保留新地址，实际 %q", got)
	}
	if got := appendForwardedFor("1.2.3.4", "5.6.7.8"); got != "1.2.3.4, 5.6.7.8" {
		t.Errorf("应追加而非覆盖，实际 %q", got)
	}
}

func TestClassifyTransportErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want contracts.Code
	}{
		{"上下文超时", context.DeadlineExceeded, contracts.CodeDownstreamTimeout},
		{"网络层超时", &net.DNSError{IsTimeout: true}, contracts.CodeDownstreamTimeout},
		{"连接被拒", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, contracts.CodeDownstreamRefused},
		{"DNS 解析失败", &net.DNSError{Err: "no such host"}, contracts.CodeDownstreamRefused},
		{"其它传输错误", errors.New("boom"), contracts.CodeDownstreamBad},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proxyErr := classify(tc.err)
			if proxyErr.Code != tc.want {
				t.Errorf("classify(%v) = %d，期望 %d", tc.err, proxyErr.Code, tc.want)
			}
			if proxyErr.Error() == "" {
				t.Error("包装后的错误应有可读信息")
			}
			if errors.Unwrap(proxyErr) == nil {
				t.Error("应支持 errors.Unwrap 取回原始错误")
			}
		})
	}
}
