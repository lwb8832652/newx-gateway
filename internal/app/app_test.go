package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"

	"newx-gateway/internal/config"
	"newx-gateway/internal/contracts"
	"newx-gateway/internal/httpx"
	"newx-gateway/internal/observability"
)

// echoPath 是测试专用的回显路由：把入口实际发给下游的请求头原样返回。
// 它是验证「客户端伪造头是否被剥离」的唯一可靠手段——从外部无法观测被转发前的请求。
const echoPath = "/__test/echo-headers"

type testEnv struct {
	baseURL string
	logs    *syncBuffer
	client  *http.Client
}

func startTestApp(t *testing.T, mutate func(*config.Config)) *testEnv {
	t.Helper()

	cfg := testConfig(t, mutate)
	logs := &syncBuffer{}
	logger, err := observability.NewLogger(cfg.LogLevel, cfg.LogFormat, logs)
	if err != nil {
		t.Fatalf("构造日志器失败: %v", err)
	}

	gateway, err := New(cfg, logger)
	if err != nil {
		t.Fatalf("装配入口失败: %v", err)
	}
	gateway.Hertz.GET(echoPath, func(_ context.Context, c *app.RequestContext) {
		httpx.WriteJSON(c, http.StatusOK, map[string]string{
			"user_id":       c.Request.Header.Get(contracts.HeaderUserID),
			"user_scope":    c.Request.Header.Get(contracts.HeaderUserScope),
			"principal_id":  c.Request.Header.Get(contracts.HeaderPrincipalID),
			"app_code":      c.Request.Header.Get(contracts.HeaderAppCode),
			"forwarded_for": c.Request.Header.Get(contracts.HeaderForwardedFor),
			"internal":      c.Request.Header.Get("X-Internal-Trace"),
			"authorization": c.Request.Header.Get(contracts.HeaderAuthorization),
		})
	})

	runErr := make(chan error, 1)
	go func() { runErr <- gateway.Run() }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := gateway.Shutdown(shutdownCtx); err != nil {
			t.Logf("关闭服务失败: %v", err)
		}
		select {
		case err := <-runErr:
			if err != nil {
				t.Logf("服务退出: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Log("等待服务退出超时")
		}
	})

	env := &testEnv{
		baseURL: "http://" + cfg.HTTPAddr,
		logs:    logs,
		client:  &http.Client{Timeout: 5 * time.Second},
	}
	env.waitHealthy(t, cfg.HTTPAddr)
	return env
}

func (e *testEnv) waitHealthy(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("服务在 5s 内未启动，日志:\n%s", e.logs.String())
}

type requestOptions struct {
	method  string
	path    string
	headers map[string]string
	body    string
}

func (e *testEnv) do(t *testing.T, options requestOptions) (*http.Response, []byte) {
	t.Helper()

	method := options.method
	if method == "" {
		method = http.MethodGet
	}
	var reader io.Reader
	if options.body != "" {
		reader = strings.NewReader(options.body)
	}
	request, err := http.NewRequest(method, e.baseURL+options.path, reader)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	for key, value := range options.headers {
		request.Header.Set(key, value)
	}

	response, err := e.client.Do(request)
	if err != nil {
		t.Fatalf("请求 %s %s 失败: %v", method, options.path, err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	return response, body
}

// decodeError 解析统一错误响应体。
func decodeError(t *testing.T, body []byte) contracts.ErrorResponse {
	t.Helper()
	var decoded contracts.ErrorResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("响应不是统一错误体: %v，原文 %s", err, body)
	}
	return decoded
}

func (e *testEnv) logEntries(t *testing.T) []map[string]any {
	t.Helper()
	var entries []map[string]any
	for _, line := range e.logs.Lines() {
		if !strings.HasPrefix(strings.TrimSpace(line), "{") {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("日志行不是合法 JSON: %v，原文 %s", err, line)
		}
		if entry["msg"] != "request" {
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}

func TestHealthEndpoints(t *testing.T) {
	env := startTestApp(t, nil)

	response, body := env.do(t, requestOptions{path: contracts.PrefixHealthz})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("/healthz 状态码 = %d，期望 200，响应 %s", response.StatusCode, body)
	}
	var health struct {
		Status    string `json:"status"`
		Version   string `json:"version"`
		Env       string `json:"env"`
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatalf("/healthz 响应解析失败: %v", err)
	}
	if health.Status != "ok" || health.Version != "test-version" || health.Env != config.EnvTest {
		t.Errorf("/healthz 响应异常: %+v", health)
	}
	if _, err := time.Parse(time.RFC3339, health.Timestamp); err != nil {
		t.Errorf("时间戳格式异常: %q", health.Timestamp)
	}

	response, body = env.do(t, requestOptions{path: contracts.PrefixReadyz})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("/readyz 状态码 = %d，期望 200，响应 %s", response.StatusCode, body)
	}
	// 默认不探测下游，因此不应出现 checks 字段。
	if strings.Contains(string(body), "checks") {
		t.Errorf("未开启下游探测时 /readyz 不应返回 checks: %s", body)
	}
}

func TestReadyzProbesDownstreamWhenEnabled(t *testing.T) {
	// 指向一个必然不可达的地址，验证探测失败时降级为 503。
	env := startTestApp(t, func(cfg *config.Config) {
		cfg.ReadyzProbeDownstream = true
		cfg.Apps = []config.AppConfig{{Code: "huobao", BaseURL: "http://127.0.0.1:1"}}
	})

	response, body := env.do(t, requestOptions{path: contracts.PrefixReadyz})
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/readyz 状态码 = %d，期望 503，响应 %s", response.StatusCode, body)
	}
	if !strings.Contains(string(body), `"degraded"`) {
		t.Errorf("/readyz 应报告 degraded: %s", body)
	}
}

func TestWebFamilyReturnsNotImplemented(t *testing.T) {
	env := startTestApp(t, nil)

	response, body := env.do(t, requestOptions{path: "/web/huobao/v1/dramas"})
	if response.StatusCode != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d，期望 501，响应 %s", response.StatusCode, body)
	}
	decoded := decodeError(t, body)
	if decoded.Code != int(contracts.CodeNotImplemented) {
		t.Errorf("错误码 = %d，期望 %d", decoded.Code, contracts.CodeNotImplemented)
	}
	if decoded.RequestID == "" {
		t.Error("错误响应必须携带 request_id")
	}
	if response.Header.Get(contracts.HeaderRequestID) != decoded.RequestID {
		t.Error("响应头 X-Request-Id 应与响应体 request_id 一致")
	}

	// 家族根路径与通配路径都要可命中。
	for _, path := range []string{"/web/huobao", "/web/huobao/v1", "/web/huobao/v1/dramas/1"} {
		if _, body := env.do(t, requestOptions{path: path}); decodeError(t, body).Code != int(contracts.CodeNotImplemented) {
			t.Errorf("路径 %s 未命中 Web 家族", path)
		}
	}
}

func TestUnknownAppIsRejected(t *testing.T) {
	env := startTestApp(t, nil)

	for _, path := range []string{"/web/unknown/v1/dramas", "/admin/unknown/v1/users", "/mcp/unknown/tools"} {
		response, body := env.do(t, requestOptions{path: path})
		if response.StatusCode != http.StatusNotFound {
			t.Errorf("路径 %s 状态码 = %d，期望 404", path, response.StatusCode)
		}
		if decoded := decodeError(t, body); decoded.Code != int(contracts.CodeAppNotFound) {
			t.Errorf("路径 %s 错误码 = %d，期望 %d", path, decoded.Code, contracts.CodeAppNotFound)
		}
	}
}

func TestClosedChannelsReportRouteNotFound(t *testing.T) {
	env := startTestApp(t, func(cfg *config.Config) {
		cfg.MCPEnabled = false
		cfg.OpenAPIEnabled = false
	})

	for _, path := range []string{"/mcp/huobao/tools", "/open-api/huobao/v1/dramas"} {
		response, body := env.do(t, requestOptions{path: path})
		if response.StatusCode != http.StatusNotFound {
			t.Errorf("关闭通道 %s 状态码 = %d，期望 404", path, response.StatusCode)
		}
		if decoded := decodeError(t, body); decoded.Code != int(contracts.CodeRouteNotFound) {
			t.Errorf("关闭通道 %s 错误码 = %d，期望 %d", path, decoded.Code, contracts.CodeRouteNotFound)
		}
	}
}

func TestEnabledFamiliesAreReachable(t *testing.T) {
	env := startTestApp(t, func(cfg *config.Config) {
		cfg.OpenAPIEnabled = true
	})

	cases := []struct {
		path   string
		family string
	}{
		{"/web/huobao/v1/dramas", contracts.FamilyWeb},
		{"/admin/huobao/v1/users", contracts.FamilyAdmin},
		{"/mcp/huobao/tools", contracts.FamilyMCP},
		{"/open-api/huobao/v1/dramas", contracts.FamilyOpenAPI},
	}

	for _, tc := range cases {
		response, body := env.do(t, requestOptions{path: tc.path})
		if response.StatusCode != http.StatusNotImplemented {
			t.Errorf("路径 %s 状态码 = %d，期望 501", tc.path, response.StatusCode)
			continue
		}
		decoded := decodeError(t, body)
		if decoded.Code != int(contracts.CodeNotImplemented) {
			t.Errorf("路径 %s 错误码 = %d", tc.path, decoded.Code)
		}
	}

	// 日志中应能看到各家族标识与下游编码。
	routes := make(map[string]bool)
	for _, entry := range env.logEntries(t) {
		if route, ok := entry["route"].(string); ok {
			routes[route] = true
		}
	}
	for _, tc := range cases {
		if !routes[tc.family] {
			t.Errorf("日志缺少家族 %q 的记录，实际 %v", tc.family, routes)
		}
	}
}

func TestFallbackForUnknownPathAndMethod(t *testing.T) {
	env := startTestApp(t, nil)

	cases := []requestOptions{
		{path: "/nope"},
		{path: "/api/v1/dramas"},
		{method: http.MethodDelete, path: contracts.PrefixHealthz},
		{method: http.MethodPost, path: contracts.PrefixHealthz},
	}

	for _, options := range cases {
		response, body := env.do(t, options)
		if response.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s 状态码 = %d，期望 404", options.method, options.path, response.StatusCode)
		}
		if decoded := decodeError(t, body); decoded.Code != int(contracts.CodeRouteNotFound) {
			t.Errorf("%s %s 错误码 = %d，期望 %d", options.method, options.path, decoded.Code, contracts.CodeRouteNotFound)
		}
	}
}

func TestForgedInternalHeadersAreStripped(t *testing.T) {
	env := startTestApp(t, nil)

	response, body := env.do(t, requestOptions{
		path: echoPath,
		headers: map[string]string{
			contracts.HeaderUserID:        "forged-user",
			contracts.HeaderUserScope:     "huobao.admin",
			contracts.HeaderPrincipalID:   "forged-principal",
			contracts.HeaderAppCode:       "forged-app",
			contracts.HeaderForwardedFor:  "10.0.0.1",
			"X-Internal-Trace":            "forged-internal",
			contracts.HeaderAuthorization: "Bearer forged-token",
		},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("回显路由状态码 = %d，响应 %s", response.StatusCode, body)
	}

	var echoed map[string]string
	if err := json.Unmarshal(body, &echoed); err != nil {
		t.Fatalf("回显响应解析失败: %v", err)
	}

	for _, field := range []string{"user_id", "user_scope", "principal_id", "app_code", "forwarded_for", "internal"} {
		if echoed[field] != "" {
			t.Errorf("伪造头 %s 未被剥离，回显值 %q", field, echoed[field])
		}
	}
	// Authorization 不在剥离清单中：鉴权中间件需要在剥离其它头之后自己消费它。
	if echoed["authorization"] != "Bearer forged-token" {
		t.Errorf("Authorization 不应在通用剥离阶段被删除，回显 %q", echoed["authorization"])
	}
}

func TestAppCodeIsInjectedByFamilyHandler(t *testing.T) {
	env := startTestApp(t, nil)

	if _, body := env.do(t, requestOptions{path: "/web/huobao/v1/dramas"}); decodeError(t, body).Code != int(contracts.CodeNotImplemented) {
		t.Fatalf("Web 路由行为异常: %s", body)
	}

	found := false
	for _, entry := range env.logEntries(t) {
		if entry["route"] == contracts.FamilyWeb && entry["app_code"] == "huobao" && entry["downstream"] == "huobao" {
			found = true
			if entry["upstream_host"] != "127.0.0.1:5679" {
				t.Errorf("upstream_host = %v，期望 127.0.0.1:5679", entry["upstream_host"])
			}
		}
	}
	if !found {
		t.Errorf("日志应记录入口注入的 app_code 与 downstream，实际:\n%s", env.logs.String())
	}
}

func TestRequestIDPropagationAndSanitizing(t *testing.T) {
	env := startTestApp(t, nil)

	// 合法标识应原样透传，保证跨系统链路可对照。
	response, _ := env.do(t, requestOptions{
		path:    contracts.PrefixHealthz,
		headers: map[string]string{contracts.HeaderRequestID: "trace-abc.123"},
	})
	if got := response.Header.Get(contracts.HeaderRequestID); got != "trace-abc.123" {
		t.Errorf("合法请求标识未透传，实际 %q", got)
	}

	// 含非法字符的标识必须被丢弃并重新生成，避免日志被注入内容污染。
	for _, forged := range []string{"bad id!!", "汉字标识", strings.Repeat("x", 128)} {
		response, _ := env.do(t, requestOptions{
			path:    contracts.PrefixHealthz,
			headers: map[string]string{contracts.HeaderRequestID: forged},
		})
		got := response.Header.Get(contracts.HeaderRequestID)
		if got == forged || len(got) != 32 {
			t.Errorf("非法标识 %q 未被替换，实际 %q", forged, got)
		}
	}
}

func TestTraceParentIsAcceptedAndRegenerated(t *testing.T) {
	env := startTestApp(t, nil)

	valid := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	env.do(t, requestOptions{
		path:    contracts.PrefixHealthz,
		headers: map[string]string{contracts.HeaderTraceParent: valid},
	})

	found := false
	for _, entry := range env.logEntries(t) {
		if entry["trace_id"] == "4bf92f3577b34da6a3ce929d0e0e4736" {
			found = true
		}
	}
	if !found {
		t.Errorf("合法 traceparent 的 trace_id 应被沿用，实际日志:\n%s", env.logs.String())
	}
}

func TestRequestBodyLimit(t *testing.T) {
	env := startTestApp(t, func(cfg *config.Config) {
		cfg.MaxBodyBytes = 1024
	})

	response, body := env.do(t, requestOptions{
		method: http.MethodPost,
		path:   "/web/huobao/v1/dramas",
		body:   strings.Repeat("a", 4096),
	})
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 = %d，期望 413，响应 %s", response.StatusCode, body)
	}
	if decoded := decodeError(t, body); decoded.Code != int(contracts.CodeBodyTooLarge) {
		t.Errorf("错误码 = %d，期望 %d", decoded.Code, contracts.CodeBodyTooLarge)
	}
}

func TestRequestLogHasFixedFields(t *testing.T) {
	env := startTestApp(t, nil)

	env.do(t, requestOptions{
		path: "/web/huobao/v1/dramas",
		headers: map[string]string{
			contracts.HeaderAuthorization: "Bearer secret-token",
			contracts.HeaderRequestID:     "log-check-id",
		},
	})

	var target map[string]any
	for _, entry := range env.logEntries(t) {
		if entry["request_id"] == "log-check-id" {
			target = entry
		}
	}
	if target == nil {
		t.Fatalf("未找到对应请求的日志，日志:\n%s", env.logs.String())
	}

	required := []string{
		"time", "level", "msg",
		"request_id", "trace_id", "app_code", "route", "method", "downstream",
		"upstream_host", "status", "code", "latency_ms", "req_bytes", "resp_bytes",
		"principal_id", "retry", "error_code",
	}
	for _, field := range required {
		if _, ok := target[field]; !ok {
			t.Errorf("日志缺少字段 %q，实际: %v", field, target)
		}
	}

	if target["status"].(float64) != 501 {
		t.Errorf("status = %v，期望 501", target["status"])
	}
	if target["code"].(float64) != float64(contracts.CodeNotImplemented) {
		t.Errorf("code = %v，期望 %d", target["code"], contracts.CodeNotImplemented)
	}
	if target["route"] != contracts.FamilyWeb {
		t.Errorf("route = %v，期望 %s", target["route"], contracts.FamilyWeb)
	}
	if target["method"] != http.MethodGet {
		t.Errorf("method = %v", target["method"])
	}

	// 敏感信息绝不能落盘。
	if strings.Contains(env.logs.String(), "secret-token") {
		t.Error("日志中出现了 Authorization 凭据")
	}
	if strings.Contains(env.logs.String(), "forged") {
		t.Error("日志中出现了被剥离的伪造头内容")
	}
}

func TestAssemblyRejectsInvalidRegistry(t *testing.T) {
	// 刻意在校验通过之后再注入重复项：配置层的校验只是第一道防线，
	// 注册表必须自己也能识别非法输入（例如未来由动态配置下发时绕过配置层）。
	cfg := testConfig(t, nil)
	cfg.Apps = []config.AppConfig{{Code: "huobao"}, {Code: "huobao"}}

	if _, err := New(cfg, nil); err == nil {
		t.Fatal("重复应用编码必须让装配失败")
	}
}

func TestProtocolTimeoutsCoverLongRunningBudgets(t *testing.T) {
	cfg := testConfig(t, nil)

	if got := protocolReadTimeout(cfg); got < cfg.Timeouts.Upload {
		t.Errorf("读取超时 %s 小于上传预算 %s", got, cfg.Timeouts.Upload)
	}
	if got := protocolWriteTimeout(cfg); got < cfg.Timeouts.Generate {
		t.Errorf("写入超时 %s 小于生成预算 %s", got, cfg.Timeouts.Generate)
	}
}

func testConfig(t *testing.T, mutate func(*config.Config)) *config.Config {
	t.Helper()

	cfg := &config.Config{
		HTTPAddr:           fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		Env:                config.EnvTest,
		Version:            "test-version",
		ExitWaitTime:       200 * time.Millisecond,
		Apps:               []config.AppConfig{{Code: "huobao", BaseURL: "http://127.0.0.1:5679"}},
		MCPEnabled:         true,
		OpenAPIEnabled:     false,
		AuthIssuerBaseURL:  "http://127.0.0.1:9099",
		MaxBodyBytes:       config.DefaultMaxBodyBytes,
		MaxBodyBytesUpload: config.DefaultMaxBodyBytesUpload,
		Timeouts: config.Timeouts{
			Read:     config.DefaultTimeoutRead,
			Write:    config.DefaultTimeoutWrite,
			Generate: config.DefaultTimeoutGenerate,
			Upload:   config.DefaultTimeoutUpload,
			Static:   config.DefaultTimeoutStatic,
			MCP:      config.DefaultTimeoutMCP,
		},
		Rates: config.RateLimits{
			Web:                    config.DefaultRateWeb,
			WebGenerate:            config.DefaultRateWebGenerate,
			Admin:                  config.DefaultRateAdmin,
			MCP:                    config.DefaultRateMCP,
			MaxConcurrencyGenerate: config.DefaultMaxConcurrencyGen,
		},
		LogLevel:  "debug",
		LogFormat: config.LogFormatJSON,
	}

	if mutate != nil {
		mutate(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("测试配置不合法: %v", err)
	}
	return cfg
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("获取空闲端口失败: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// syncBuffer 是并发安全的日志缓冲：日志由多个连接的处理协程写入，测试在主协程读取。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Lines() []string {
	content := b.String()
	if content == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(content, "\n"), "\n")
}
