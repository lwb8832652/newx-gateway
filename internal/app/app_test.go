package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"

	"newx-gateway/internal/auth"
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

func TestDiscoveryPreservesHTTPSAndDefaultPorts(t *testing.T) {
	cfg := testConfig(t, nil)
	cfg.Apps = []config.AppConfig{
		{Code: "secure", BaseURL: "https://example.com"},
		{Code: "plain", BaseURL: "http://example.com"},
	}
	resolver, cancel, err := buildResolver(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cancel != nil {
		defer cancel()
	}
	for appCode, want := range map[string]string{"secure": "https://example.com:443", "plain": "http://example.com:80"} {
		instances, err := resolver.Resolve(context.Background(), appCode)
		if err != nil || len(instances) != 1 || instances[0].URL() != want {
			t.Errorf("%s 解析错误: %v, %v", appCode, instances, err)
		}
	}
}

func TestReadyzProbesNacosInstances(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer slow.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadURL := "http://" + listener.Addr().String()
	listener.Close()

	cases := []struct {
		name      string
		endpoints []string
		want      int
	}{
		{"reachable", []string{upstream.URL}, http.StatusOK},
		{"unreachable", []string{deadURL}, http.StatusServiceUnavailable},
		{"no_instances", nil, http.StatusServiceUnavailable},
		{"healthy_fallback", []string{deadURL, upstream.URL}, http.StatusOK},
		{"slow_first_healthy_fallback", []string{slow.URL, upstream.URL}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nacos := mockNacos(t, "yudao-system", tc.endpoints...)
			defer nacos.Close()
			env := startTestApp(t, func(cfg *config.Config) {
				cfg.ReadyzProbeDownstream = true
				cfg.Apps = []config.AppConfig{{Code: "system", Service: "yudao-system"}}
				cfg.Discovery = config.Discovery{Enabled: true, Addr: strings.TrimPrefix(nacos.URL, "http://"), Refresh: time.Minute}
			})
			response, body := env.do(t, requestOptions{path: "/readyz"})
			if response.StatusCode != tc.want {
				t.Fatalf("状态码 = %d，期望 %d: %s", response.StatusCode, tc.want, body)
			}
			var ready struct {
				Checks []struct {
					AppCode   string `json:"app_code"`
					Reachable bool   `json:"reachable"`
					Error     string `json:"error"`
				} `json:"checks"`
			}
			if err := json.Unmarshal(body, &ready); err != nil {
				t.Fatalf("解析就绪响应失败: %v", err)
			}
			if len(ready.Checks) != 1 || ready.Checks[0].AppCode != "system" {
				t.Fatalf("就绪检查结果错误: %s", body)
			}
			check := ready.Checks[0]
			if check.Reachable != (tc.want == http.StatusOK) || (!check.Reachable && check.Error == "") {
				t.Errorf("就绪检查结果错误: %+v", check)
			}
		})
	}
}

func TestNoNacosInstancesWithoutBreakerReturnsUnavailable(t *testing.T) {
	nacos := mockNacos(t, "yudao-system")
	defer nacos.Close()
	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "system", Service: "yudao-system"}}
		cfg.Discovery = config.Discovery{Enabled: true, Addr: strings.TrimPrefix(nacos.URL, "http://"), Refresh: time.Minute}
		cfg.Breaker.Enabled = false
	})
	response, body := env.do(t, requestOptions{path: "/web/system/v1/users"})
	if response.StatusCode != contracts.CodeDownstreamRefused.HTTPStatus() || !strings.Contains(string(body), `"code":50201`) {
		t.Fatalf("无实例应正常返回50201，而非panic: %d, %s", response.StatusCode, body)
	}
}

func TestAuthUsesNacosServiceAlias(t *testing.T) {
	var checks atomic.Int32
	checkServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checks.Add(1)
		if r.URL.Path != "/rpc-api/system/oauth2/token/check" || r.Header.Get("tenant-id") != "7" {
			t.Errorf("校验请求错误: %s, tenant-id=%q", r.URL.Path, r.Header.Get("tenant-id"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{"userId":42,"userType":1,"tenantId":7,"expiresTime":4070908800000}}`)
	}))
	defer checkServer.Close()
	nacos := mockNacos(t, "yudao-system", checkServer.URL)
	defer nacos.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := auth.ParseLoginUser(r.Header.Get("login-user"))
		if err != nil || user == nil || user.ID != 42 {
			t.Errorf("下游未收到有效身份: %v, %v", user, err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "huobao", BaseURL: upstream.URL}, {Code: "system", Service: "yudao-system"}}
		cfg.Discovery = config.Discovery{Enabled: true, Addr: strings.TrimPrefix(nacos.URL, "http://"), Refresh: time.Minute}
		cfg.Auth = config.Auth{Enabled: true, AppCode: "system", CacheTTL: time.Minute, Timeout: time.Second}
	})
	for range 2 {
		response, _ := env.do(t, requestOptions{
			path: "/web/huobao/v1/dramas",
			headers: map[string]string{
				contracts.HeaderAuthorization: "Bearer alias-token",
				contracts.HeaderTenantID:      "7",
			},
		})
		if response.StatusCode != http.StatusOK {
			t.Errorf("状态码 = %d，期望 200", response.StatusCode)
		}
	}
	if checks.Load() != 1 {
		t.Errorf("check-token 调用 %d 次，期望 1", checks.Load())
	}
}

func mockNacos(t *testing.T, service string, endpoints ...string) *httptest.Server {
	t.Helper()
	hosts := make([]map[string]any, 0, len(endpoints))
	for _, endpoint := range endpoints {
		host, port, err := net.SplitHostPort(strings.TrimPrefix(endpoint, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		hosts = append(hosts, map[string]any{
			"ip": host, "port": json.Number(port), "healthy": true, "enabled": true, "weight": 1,
		})
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("serviceName"); got != service {
			t.Errorf("Nacos 查询服务名 = %q，期望 %q", got, service)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"hosts": hosts})
	}))
}

func TestProxyForwardsRequestAndResponse(t *testing.T) {
	var (
		gotMethod    string
		gotPath      string
		gotQuery     string
		gotAppCode   string
		gotUserID    string
		gotRequestID string
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAppCode = r.Header.Get(contracts.HeaderAppCode)
		gotUserID = r.Header.Get(contracts.HeaderUserID)
		gotRequestID = r.Header.Get(contracts.HeaderRequestID)

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Marker", "from-upstream")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true,"path":"` + r.URL.Path + `"}`))
	}))
	defer upstream.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "huobao", BaseURL: upstream.URL}}
	})

	response, body := env.do(t, requestOptions{
		method: http.MethodPost,
		path:   "/web/huobao/v1/dramas?page=2&size=10",
		headers: map[string]string{
			contracts.HeaderRequestID: "proxy-check-id",
			contracts.HeaderUserID:    "forged-user",
		},
		body: `{"name":"x"}`,
	})

	if response.StatusCode != http.StatusCreated {
		t.Fatalf("下游状态码应被透传，实际 %d，响应 %s", response.StatusCode, body)
	}
	if got := response.Header.Get("X-Upstream-Marker"); got != "from-upstream" {
		t.Errorf("下游响应头应被透传，实际 %q", got)
	}
	if !strings.Contains(string(body), `"path":"/dramas"`) {
		t.Errorf("下游响应体应被原样回写，实际 %s", body)
	}

	// 入口注入的可信头必须到达下游。
	if gotMethod != http.MethodPost {
		t.Errorf("上游方法 = %q，期望 POST", gotMethod)
	}
	if gotPath != "/dramas" {
		t.Errorf("上游路径 = %q，期望 /dramas", gotPath)
	}
	if gotQuery != "page=2&size=10" {
		t.Errorf("上游 Query = %q，应原样透传", gotQuery)
	}
	if gotAppCode != "huobao" {
		t.Errorf("上游 app_code = %q，期望 huobao", gotAppCode)
	}
	if gotRequestID != "proxy-check-id" {
		t.Errorf("上游 request_id = %q，期望 proxy-check-id", gotRequestID)
	}
	// 客户端伪造的身份头必须在到达上游前被剥离。
	if gotUserID != "" {
		t.Errorf("伪造的 X-User-Id 被透传到了下游: %q", gotUserID)
	}
}

func TestProxyRewritesUpstreamPath(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	// 入口 /admin/yudao-system/v1/user/page → 上游 /admin-api/system/user/page
	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{
			Code:         "yudao-system",
			BaseURL:      upstream.URL,
			UpstreamPath: "/admin-api/system",
		}}
	})

	response, _ := env.do(t, requestOptions{path: "/admin/yudao-system/v1/user/page"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", response.StatusCode)
	}
	if gotPath != "/admin-api/system/user/page" {
		t.Errorf("上游路径 = %q，期望 /admin-api/system/user/page", gotPath)
	}

	// 家族根路径也应正确改写为上游前缀本身。
	response, _ = env.do(t, requestOptions{path: "/admin/yudao-system"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("根路径状态码 = %d，期望 200", response.StatusCode)
	}
	if gotPath != "/admin-api/system" {
		t.Errorf("根路径上游 = %q，期望 /admin-api/system", gotPath)
	}
}

func TestProxyReportsUnreachableDownstream(t *testing.T) {
	// 取一个必然无人监听的端口。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("获取空闲端口失败: %v", err)
	}
	deadAddr := "http://" + listener.Addr().String()
	listener.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "huobao", BaseURL: deadAddr}}
	})

	response, body := env.do(t, requestOptions{path: "/web/huobao/v1/dramas"})
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503，响应 %s", response.StatusCode, body)
	}
	if decoded := decodeError(t, body); decoded.Code != int(contracts.CodeDownstreamRefused) {
		t.Errorf("错误码 = %d，期望 %d（下游不可用）", decoded.Code, contracts.CodeDownstreamRefused)
	}
}

func TestProxyPassesThroughDownstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":500,"message":"upstream boom"}`))
	}))
	defer upstream.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "huobao", BaseURL: upstream.URL}}
	})

	response, body := env.do(t, requestOptions{path: "/web/huobao/v1/dramas"})

	// 下游错误响应必须原样透传：客户端才能按自己的错误码体系处理。
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", response.StatusCode)
	}
	if !strings.Contains(string(body), "upstream boom") {
		t.Errorf("下游错误响应体应被透传，实际 %s", body)
	}

	// 但错误码归类必须记为 50202，供日志与告警识别。
	found := false
	for _, entry := range env.logEntries(t) {
		if entry["route"] == contracts.FamilyWeb {
			found = true
			if int(entry["error_code"].(float64)) != int(contracts.CodeDownstreamBad) {
				t.Errorf("error_code = %v，期望 %d", entry["error_code"], contracts.CodeDownstreamBad)
			}
			if int(entry["status"].(float64)) != 500 {
				t.Errorf("status = %v，期望 500", entry["status"])
			}
		}
	}
	if !found {
		t.Error("缺少 Web 家族的请求日志")
	}
}

// TestProxyStreamsLargeRequestBody 验证大请求体不再整体读进内存：
// 入口以流式转发，上游应完整收到数 MB 的请求体。
func TestProxyStreamsLargeRequestBody(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 6<<20) // 6MB
	var received int64

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		received = n
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"received":` + strconv.FormatInt(n, 10) + `}`))
	}))
	defer upstream.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "huobao", BaseURL: upstream.URL, UpstreamPath: "/api/v1"}}
		// 上传路径命中上传档，避免被读/写预算掐断。
		cfg.TimeoutRules = config.TimeoutRules{GeneratePaths: []string{"/agent"}, UploadPaths: []string{"/upload"}}
	})

	response, body := env.do(t, requestOptions{
		method: http.MethodPost,
		path:   "/web/huobao/v1/upload",
		body:   string(payload),
	})

	if response.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，响应 %s", response.StatusCode, body)
	}
	if received != int64(len(payload)) {
		t.Errorf("上游收到 %d 字节，期望 %d（流式转发丢数据）", received, len(payload))
	}
	if !strings.Contains(string(body), strconv.FormatInt(int64(len(payload)), 10)) {
		t.Errorf("响应体异常: %s", body)
	}
}

// TestProxyPreservesPathEncoding 验证路径中的百分号编码被原样透传。
func TestProxyPreservesPathEncoding(t *testing.T) {
	var gotPath, gotRawPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRawPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "huobao", BaseURL: upstream.URL, UpstreamPath: "/api/v1"}}
	})

	// 含中文与 %2F 的路径：解码后转发会导致下游路径匹配错位。
	env.do(t, requestOptions{path: "/web/huobao/v1/dramas/%E4%B8%AD%E6%96%87"})

	if gotRawPath != "/api/v1/dramas/%E4%B8%AD%E6%96%87" {
		t.Errorf("上游收到转义路径 %q，期望 %q（编码未被保留）", gotRawPath, "/api/v1/dramas/%E4%B8%AD%E6%96%87")
	}
	if gotPath != "/api/v1/dramas/中文" {
		t.Errorf("上游解码路径 = %q，期望 %q", gotPath, "/api/v1/dramas/中文")
	}
}

// TestProxyRetriesIdempotentRequests 验证幂等请求在传输层失败后会重试一次。
func TestProxyRetriesIdempotentRequests(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			// 首次请求直接掐断连接，模拟连接级抖动。
			if hijacker, ok := w.(http.Hijacker); ok {
				if conn, _, err := hijacker.Hijack(); err == nil {
					_ = conn.Close()
					return
				}
			}
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"attempt":"second"}`))
	}))
	defer upstream.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "huobao", BaseURL: upstream.URL, UpstreamPath: "/api/v1"}}
	})

	response, body := env.do(t, requestOptions{path: "/web/huobao/v1/dramas"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（重试未生效），响应 %s", response.StatusCode, body)
	}
	if !strings.Contains(string(body), "second") {
		t.Errorf("应使用重试后的响应，实际 %s", body)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("上游被请求 %d 次，期望 2（一次失败 + 一次重试）", got)
	}
}

func TestProxyRetryExcludesUnsafeRequests(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer upstream.Close()
	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "huobao", BaseURL: upstream.URL, UpstreamPath: "/api/v1"}}
		cfg.Breaker.Enabled = false
		cfg.TimeoutRules.GeneratePaths = append(cfg.TimeoutRules.GeneratePaths, "/custom-job")
	})
	for _, tc := range []struct {
		method string
		path   string
		want   int32
	}{
		{http.MethodGet, "/dramas", 2},
		{http.MethodHead, "/dramas", 2},
		{http.MethodOptions, "/dramas", 2},
		{http.MethodGet, "/agent/run", 1},
		{http.MethodHead, "/tasks/start", 1},
		{http.MethodOptions, "/merge/start", 1},
		{http.MethodGet, "/previs/preview", 1},
		{http.MethodGet, "/task-queue/create", 1},
		{http.MethodGet, "/custom-job/start", 1},
		{http.MethodGet, "/%74asks/start", 1},
		{http.MethodGet, "/upload", 1},
		{http.MethodPost, "/dramas", 1},
		{http.MethodPut, "/dramas/1", 1},
		{http.MethodDelete, "/dramas/1", 1},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			before := attempts.Load()
			response, _ := env.do(t, requestOptions{
				method: tc.method, path: "/web/huobao/v1" + tc.path,
				headers: map[string]string{"Idempotency-Key": "test-key"},
			})
			if response.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("状态码 = %d，期望 503", response.StatusCode)
			}
			if got := attempts.Load() - before; got != tc.want {
				t.Errorf("请求次数 = %d，期望 %d", got, tc.want)
			}
		})
	}
}

// TestProxyDoesNotRetryNonIdempotentRequests 验证写方法不自动重试。
func TestProxyDoesNotRetryNonIdempotentRequests(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if hijacker, ok := w.(http.Hijacker); ok {
			if conn, _, err := hijacker.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "huobao", BaseURL: upstream.URL, UpstreamPath: "/api/v1"}}
	})

	response, _ := env.do(t, requestOptions{
		method: http.MethodPost,
		path:   "/web/huobao/v1/dramas",
		body:   `{"name":"x"}`,
	})
	if response.StatusCode == http.StatusOK {
		t.Fatal("写方法失败时不应重试成功")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("上游被请求 %d 次，期望 1（写方法不得自动重试）", got)
	}
}

// TestProxyUploadChannelExemptFromBodyLimit 验证上传通道不受普通体积上限约束。
func TestProxyUploadChannelExemptFromBodyLimit(t *testing.T) {
	var received int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		received = n
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "huobao", BaseURL: upstream.URL, UpstreamPath: "/api/v1"}}
		// 普通通道只有 1KB，上传通道 64MB。
		cfg.MaxBodyBytes = 1024
		cfg.MaxBodyBytesUpload = 64 << 20
		cfg.TimeoutRules = config.TimeoutRules{GeneratePaths: []string{"/agent"}, UploadPaths: []string{"/upload"}}
	})

	payload := strings.Repeat("y", 8192)

	// 普通路径：超出 1KB 必须被拒。
	response, body := env.do(t, requestOptions{
		method: http.MethodPost,
		path:   "/web/huobao/v1/dramas",
		body:   payload,
	})
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("普通路径状态码 = %d，期望 413", response.StatusCode)
	}
	if decoded := decodeError(t, body); decoded.Code != int(contracts.CodeBodyTooLarge) {
		t.Errorf("错误码 = %d，期望 %d", decoded.Code, contracts.CodeBodyTooLarge)
	}

	// 上传路径：同样大小必须放行并完整转发。
	response, _ = env.do(t, requestOptions{
		method: http.MethodPost,
		path:   "/web/huobao/v1/upload",
		body:   payload,
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("上传路径状态码 = %d，期望 200（上传通道未被豁免）", response.StatusCode)
	}
	if received != int64(len(payload)) {
		t.Errorf("上游收到 %d 字节，期望 %d", received, len(payload))
	}
}

// TestProxyCircuitBreakerStopsHammeringFailedDownstream 验证下游持续 5xx 时熔断生效。
func TestProxyCircuitBreakerStopsHammeringFailedDownstream(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "huobao", BaseURL: upstream.URL, UpstreamPath: "/api/v1"}}
		// 阈值 2：两次 5xx 后打开熔断。
		cfg.Breaker = config.Breaker{Enabled: true, FailureThreshold: 2, Cooldown: time.Minute}
	})

	for i := 0; i < 4; i++ {
		response, _ := env.do(t, requestOptions{path: "/web/huobao/v1/dramas"})
		if response.StatusCode != http.StatusInternalServerError && response.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("第 %d 次请求状态码 = %d", i+1, response.StatusCode)
		}
	}

	// 前两次真正打到下游，之后被熔断拦下，不再继续发送。
	if got := hits.Load(); got != 2 {
		t.Errorf("下游被请求 %d 次，期望 2（熔断后应停止发送）", got)
	}
}

// TestAuthInjectsYudaoLoginUserHeader 端到端验证 yudao 认证方式：
// 校验成功 → 下游收到 login-user（JSON + URL-encode）与 tenant-id 头。
func TestAuthInjectsYudaoLoginUserHeader(t *testing.T) {
	checkServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rpc-api/system/oauth2/token/check" {
			t.Errorf("校验路径 = %q，期望 /rpc-api/system/oauth2/token/check", r.URL.Path)
		}
		if got := r.Header.Get("tenant-id"); got != "99" {
			t.Errorf("check-token 租户提示 = %q，期望 99", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"userId":42,"userType":1,"userInfo":{"nickname":"管理员"},"tenantId":7,"scopes":["system:user:query"],"expiresTime":4070908800000}}`))
	}))
	defer checkServer.Close()

	var gotLoginUser, gotTenantID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLoginUser = r.Header.Get("login-user")
		gotTenantID = r.Header.Get("tenant-id")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "huobao", BaseURL: upstream.URL, UpstreamPath: "/api/v1"}}
		cfg.Auth = config.Auth{
			Enabled:   true,
			BaseURL:   checkServer.URL,
			CheckPath: "/rpc-api/system/oauth2/token/check",
			CacheTTL:  time.Minute,
			Timeout:   2 * time.Second,
		}
	})

	response, _ := env.do(t, requestOptions{
		path: "/web/huobao/v1/dramas",
		headers: map[string]string{
			contracts.HeaderAuthorization: "Bearer valid-token",
			contracts.HeaderTenantID:      "99",
		},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", response.StatusCode)
	}

	if gotTenantID != "7" {
		t.Errorf("tenant-id = %q，期望 7（来自 check-token 返回，而非客户端）", gotTenantID)
	}

	// login-user 必须是 yudao 的私有格式：JSON 再 URL-encode，且字段名一致。
	parsed, err := auth.ParseLoginUser(gotLoginUser)
	if err != nil {
		t.Fatalf("login-user 头无法解析: %v，原值 %q", err, gotLoginUser)
	}
	if parsed.ID != 42 || parsed.TenantID != 7 || parsed.UserType != 1 {
		t.Errorf("login-user 字段异常: %+v", parsed)
	}
	if parsed.Info["nickname"] != "管理员" {
		t.Errorf("login-user info 异常: %+v", parsed.Info)
	}
	if len(parsed.Scopes) != 1 || parsed.Scopes[0] != "system:user:query" {
		t.Errorf("login-user scopes 异常: %+v", parsed.Scopes)
	}
	decoded, err := url.QueryUnescape(gotLoginUser)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(decoded), &raw); err != nil {
		t.Fatal(err)
	}
	if got := string(raw["expiresTime"]); got != "4070908800000" {
		t.Errorf("expiresTime = %s，期望 yudao 毫秒时间戳 4070908800000", got)
	}
}

// TestAuthStripsForgedLoginUser 验证客户端伪造的 login-user 被替换为校验后的身份。
func TestAuthStripsForgedLoginUser(t *testing.T) {
	checkServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"userId":42,"userType":1,"tenantId":7,"expiresTime":"2099-01-01T00:00:00"}}`))
	}))
	defer checkServer.Close()

	var gotLoginUser string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLoginUser = r.Header.Get("login-user")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "huobao", BaseURL: upstream.URL, UpstreamPath: "/api/v1"}}
		cfg.Auth = config.Auth{Enabled: true, BaseURL: checkServer.URL, CacheTTL: time.Minute, Timeout: 2 * time.Second}
	})

	// 客户端伪造一个 admin 身份，必须被入口剥掉，替换为真实校验结果。
	env.do(t, requestOptions{
		path: "/web/huobao/v1/dramas",
		headers: map[string]string{
			contracts.HeaderAuthorization: "Bearer valid-token",
			contracts.HeaderLoginUser:     `{"id":999,"tenantId":1}`,
			contracts.HeaderTenantID:      "1",
		},
	})

	parsed, err := auth.ParseLoginUser(gotLoginUser)
	if err != nil {
		t.Fatalf("login-user 解析失败: %v，原值 %q", err, gotLoginUser)
	}
	if parsed.ID != 42 {
		t.Errorf("伪造身份未被剥离，下游收到 id=%d", parsed.ID)
	}
}

// TestAuthPassesThroughWithoutToken 与 TestAuthPassesThroughOnInvalidToken
// 验证 yudao 的核心语义：网关不拦截，是否需要登录由下游判断。
func TestAuthPassesThroughWithoutToken(t *testing.T) {
	checkServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("无令牌时不应调用 check-token")
		w.WriteHeader(http.StatusOK)
	}))
	defer checkServer.Close()

	var gotLoginUser, gotTenantID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLoginUser = r.Header.Get("login-user")
		gotTenantID = r.Header.Get("tenant-id")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "huobao", BaseURL: upstream.URL, UpstreamPath: "/api/v1"}}
		cfg.Auth = config.Auth{Enabled: true, BaseURL: checkServer.URL, CacheTTL: time.Minute, Timeout: 2 * time.Second}
	})

	response, _ := env.do(t, requestOptions{
		path: "/web/huobao/v1/dramas",
		headers: map[string]string{
			contracts.HeaderTenantID:  "7",
			contracts.HeaderLoginUser: `{"id":999,"tenantId":1}`,
		},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（无令牌应放行）", response.StatusCode)
	}
	if gotLoginUser != "" || gotTenantID != "7" {
		t.Errorf("匿名请求应保留租户提示但剥离身份，login-user=%q, tenant-id=%q", gotLoginUser, gotTenantID)
	}
}

func TestAuthPassesThroughOnInvalidToken(t *testing.T) {
	checkServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":401,"data":null,"message":"令牌已过期"}`))
	}))
	defer checkServer.Close()

	var gotLoginUser, gotTenantID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLoginUser = r.Header.Get("login-user")
		gotTenantID = r.Header.Get("tenant-id")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "huobao", BaseURL: upstream.URL, UpstreamPath: "/api/v1"}}
		cfg.Auth = config.Auth{Enabled: true, BaseURL: checkServer.URL, CacheTTL: time.Minute, Timeout: 2 * time.Second}
	})

	response, _ := env.do(t, requestOptions{
		path: "/web/huobao/v1/dramas",
		headers: map[string]string{
			contracts.HeaderAuthorization: "Bearer expired-token",
			contracts.HeaderTenantID:      "7",
			contracts.HeaderLoginUser:     `{"id":999,"tenantId":1}`,
		},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（令牌失效应放行，由下游判断）", response.StatusCode)
	}
	if gotLoginUser != "" || gotTenantID != "7" {
		t.Errorf("令牌失效应保留租户提示但不注入身份，login-user=%q, tenant-id=%q", gotLoginUser, gotTenantID)
	}
}

// TestAuthFailsOpenWhenIssuerUnreachable 验证鉴权方不可用时按无身份放行，
// 入口不因为认证链路抖动变成单点。
func TestAuthFailsOpenWhenIssuerUnreachable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("获取空闲端口失败: %v", err)
	}
	deadAuth := "http://" + listener.Addr().String()
	listener.Close()

	var gotLoginUser string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLoginUser = r.Header.Get("login-user")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "huobao", BaseURL: upstream.URL, UpstreamPath: "/api/v1"}}
		cfg.Auth = config.Auth{Enabled: true, BaseURL: deadAuth, CacheTTL: time.Minute, Timeout: 500 * time.Millisecond}
	})

	response, _ := env.do(t, requestOptions{
		path:    "/web/huobao/v1/dramas",
		headers: map[string]string{contracts.HeaderAuthorization: "Bearer whatever"},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（鉴权方不可用应放行）", response.StatusCode)
	}
	if gotLoginUser != "" {
		t.Error("鉴权失败时不应注入身份")
	}
}

// TestAuthUsesCacheToAvoidHammeringIssuer 验证缓存生效：
// 同一令牌重复请求只触发一次 check-token。
func TestAuthUsesCacheToAvoidHammeringIssuer(t *testing.T) {
	var checks atomic.Int32
	checkServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checks.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"userId":42,"tenantId":7,"expiresTime":"2099-01-01T00:00:00"}}`))
	}))
	defer checkServer.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "huobao", BaseURL: upstream.URL, UpstreamPath: "/api/v1"}}
		cfg.Auth = config.Auth{Enabled: true, BaseURL: checkServer.URL, CacheTTL: time.Minute, Timeout: 2 * time.Second}
	})

	for i := 0; i < 5; i++ {
		response, _ := env.do(t, requestOptions{
			path:    "/web/huobao/v1/dramas",
			headers: map[string]string{contracts.HeaderAuthorization: "Bearer cached-token"},
		})
		if response.StatusCode != http.StatusOK {
			t.Fatalf("第 %d 次请求状态码 = %d", i+1, response.StatusCode)
		}
	}

	if got := checks.Load(); got != 1 {
		t.Errorf("check-token 被调用 %d 次，期望 1（缓存未生效）", got)
	}
}

func TestProxySupportsRangeRequests(t *testing.T) {
	payload := bytes.Repeat([]byte("abcdefghij"), 100)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "video.mp4", time.Now(), bytes.NewReader(payload))
	}))
	defer upstream.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "huobao", BaseURL: upstream.URL, UpstreamPath: "/static"}}
	})

	response, body := env.do(t, requestOptions{
		path:    "/web/huobao/v1/video.mp4",
		headers: map[string]string{"Range": "bytes=0-99"},
	})

	if response.StatusCode != http.StatusPartialContent {
		t.Fatalf("状态码 = %d，期望 206（Range 透传失败）", response.StatusCode)
	}
	if len(body) != 100 {
		t.Errorf("响应体长度 = %d，期望 100（Range 未被正确处理）", len(body))
	}
	if got := response.Header.Get("Content-Range"); got == "" {
		t.Error("Content-Range 响应头应被透传")
	}
}

func TestProxyStreamsEventsOverTCP(t *testing.T) {
	release := make(chan struct{})
	finish := sync.OnceFunc(func() { close(release) })
	defer finish()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, "data: second\n\n")
			w.(http.Flusher).Flush()
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(upstream.Close)
	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "huobao", BaseURL: upstream.URL}}
		cfg.Timeouts.Read = 2 * time.Second
	})
	response, err := env.client.Get(env.baseURL + "/web/huobao/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	first := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(response.Body, first); err != nil {
		t.Fatalf("上游未结束前未收到首事件: %v", err)
	}
	if string(first) != "data: first\n\n" {
		t.Fatalf("首事件错误: %q", first)
	}
	finish()
	rest, err := io.ReadAll(response.Body)
	if err != nil || string(rest) != "data: second\n\n" {
		t.Fatalf("后续事件未完整透传: %q, %v", rest, err)
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("流式响应头错误: %d, %v", response.StatusCode, response.Header)
	}
}

func TestProxyStreamBudgetOverTCP(t *testing.T) {
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	t.Cleanup(upstream.Close)
	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "huobao", BaseURL: upstream.URL}}
		cfg.Timeouts.Read = 400 * time.Millisecond
	})
	response, err := env.client.Get(env.baseURL + "/web/huobao/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || string(body) != "data: first\n\n" {
		t.Errorf("流中超时不得改写已发送状态或追加错误JSON: %d, %q", response.StatusCode, body)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("流预算到期后未取消上游请求")
	}
}

func TestProxyClientDisconnectCancelsUpstream(t *testing.T) {
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	t.Cleanup(upstream.Close)
	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "huobao", BaseURL: upstream.URL}}
		cfg.Timeouts.Read = 5 * time.Second
	})
	response, err := env.client.Get(env.baseURL + "/web/huobao/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	first := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(response.Body, first); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("客户端断开未及时取消上游，不能等待5s预算才回收")
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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Served-By", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "huobao", BaseURL: upstream.URL}}
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
		response, _ := env.do(t, requestOptions{path: tc.path})
		if response.StatusCode != http.StatusOK {
			t.Errorf("路径 %s 状态码 = %d，期望 200（应被转发到上游）", tc.path, response.StatusCode)
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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "huobao", BaseURL: upstream.URL}}
	})

	if response, _ := env.do(t, requestOptions{path: "/web/huobao/v1/dramas"}); response.StatusCode != http.StatusOK {
		t.Fatalf("Web 路由应成功转发，实际 %d", response.StatusCode)
	}

	found := false
	for _, entry := range env.logEntries(t) {
		if entry["route"] == contracts.FamilyWeb && entry["app_code"] == "huobao" && entry["downstream"] == "huobao" {
			found = true
			// upstream_host 应记录实际选中的下游实例，而非注册表里的静态地址。
			host, _ := entry["upstream_host"].(string)
			if host == "" || host == "127.0.0.1:5679" {
				t.Errorf("upstream_host = %q，应为实际选中的下游实例", host)
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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "huobao", BaseURL: upstream.URL, UpstreamPath: "/api/v1"}}
	})

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

	// 转发成功的请求：入口未产生错误，因此 code/error_code 为 0，status 透传下游。
	if target["status"].(float64) != 200 {
		t.Errorf("status = %v，期望 200", target["status"])
	}
	if target["code"].(float64) != 0 {
		t.Errorf("code = %v，期望 0（入口未产生错误）", target["code"])
	}
	if target["route"] != contracts.FamilyWeb {
		t.Errorf("route = %v，期望 %s", target["route"], contracts.FamilyWeb)
	}
	if target["method"] != http.MethodGet {
		t.Errorf("method = %v", target["method"])
	}
	if target["app_code"] != "huobao" {
		t.Errorf("app_code = %v，期望 huobao", target["app_code"])
	}
	if _, ok := target["upstream_host"].(string); !ok {
		t.Errorf("upstream_host 应为字符串，实际 %v", target["upstream_host"])
	}

	// 敏感信息绝不能落盘。
	if strings.Contains(env.logs.String(), "secret-token") {
		t.Error("日志中出现了 Authorization 凭据")
	}
	if strings.Contains(env.logs.String(), "forged") {
		t.Error("日志中出现了被剥离的伪造头内容")
	}
}

func TestRequestLogsExcludeSensitiveData(t *testing.T) {
	const secret = "log-sensitive-marker"
	for _, authStatus := range []int{http.StatusOK, http.StatusBadGateway} {
		t.Run(http.StatusText(authStatus), func(t *testing.T) {
			authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if authStatus != http.StatusOK {
					w.WriteHeader(authStatus)
					_, _ = io.WriteString(w, secret)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"code": 0, "data": map[string]any{
						"userId": 42, "userType": 1, "tenantId": 1,
						"userInfo":    map[string]any{"nickname": secret},
						"expiresTime": time.Now().Add(time.Hour).UnixMilli(),
					},
				})
			}))
			defer authServer.Close()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				identity := r.Header.Get(contracts.HeaderLoginUser)
				if authStatus == http.StatusOK {
					user, err := auth.ParseLoginUser(identity)
					if err != nil || user.ID != 42 || user.Info["nickname"] != secret {
						t.Errorf("有效认证身份未注入下游: %v", err)
					}
				} else if identity != "" {
					t.Error("认证失败后不得透传伪造身份")
				}
				_, _ = io.Copy(io.Discard, r.Body)
				_, _ = io.WriteString(w, secret)
			}))
			defer upstream.Close()
			env := startTestApp(t, func(cfg *config.Config) {
				cfg.Apps = []config.AppConfig{{Code: "huobao", BaseURL: upstream.URL, UpstreamPath: "/api/v1"}}
				cfg.Auth.Enabled = true
				cfg.Auth.BaseURL = strings.Replace(authServer.URL, "://", "://"+secret+"@", 1)
				cfg.Auth.CacheTTL = time.Minute
				cfg.Auth.Timeout = time.Second
			})
			response, _ := env.do(t, requestOptions{
				method: http.MethodPost, path: "/web/huobao/v1/dramas?token=" + secret,
				body: secret,
				headers: map[string]string{
					contracts.HeaderAuthorization: "Bearer " + secret,
					contracts.HeaderLoginUser:     secret, contracts.HeaderRequestID: "sensitive-log-check",
				},
			})
			if response.StatusCode != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200", response.StatusCode)
			}
			if strings.Contains(env.logs.String(), secret) {
				t.Fatal("Token、查询参数、身份信息或请求/响应正文进入日志")
			}
			found := false
			for _, entry := range env.logEntries(t) {
				if entry["request_id"] == "sensitive-log-check" && entry["msg"] == "request" {
					found = true
				}
			}
			if !found {
				t.Fatal("脱敏后仍必须保留请求关联日志")
			}
		})
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

// TestLegacyRoutesForwardOriginalPath 验证 yudao 旧路径兼容：
// 命中 LEGACY_ROUTES 后路径与查询参数原样转发，不做任何改写。
func TestLegacyRoutesForwardOriginalPath(t *testing.T) {
	var gotPath, gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "yudao-system", BaseURL: upstream.URL}}
		cfg.LegacyRoutes = []config.LegacyRoute{{Prefix: "/admin-api/system", AppCode: "yudao-system"}}
	})

	response, body := env.do(t, requestOptions{path: "/admin-api/system/user/page?pageNo=2"})
	if response.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("状态码 = %d，响应 = %s", response.StatusCode, body)
	}
	if gotPath != "/admin-api/system/user/page" {
		t.Errorf("下游路径 = %q，期望原样保留 /admin-api/system/user/page", gotPath)
	}
	if gotQuery != "pageNo=2" {
		t.Errorf("下游查询串 = %q，期望 pageNo=2", gotQuery)
	}
}

// TestLegacyRoutesCoexistWithAdminFamily 验证旧路径 /admin/** 与新区族
// /admin/{app_code}/v1/** 能同时注册：静态前缀优先于参数段。
func TestLegacyRoutesCoexistWithAdminFamily(t *testing.T) {
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{
			Code: "yudao-system", BaseURL: upstream.URL, UpstreamPath: "/admin-api",
		}}
		cfg.LegacyRoutes = []config.LegacyRoute{{Prefix: "/admin", AppCode: "yudao-system"}}
	})

	// 旧路径（Spring Boot Admin 这类入口）原样转发。
	if response, _ := env.do(t, requestOptions{path: "/admin/applications"}); response.StatusCode != http.StatusOK {
		t.Fatalf("旧路径状态码 = %d，期望 200", response.StatusCode)
	}
	// 新路径仍然走参数段路由，并拼上 UpstreamPath。
	if response, _ := env.do(t, requestOptions{path: "/admin/yudao-system/v1/system/user/get"}); response.StatusCode != http.StatusOK {
		t.Fatalf("新路径状态码 = %d，期望 200", response.StatusCode)
	}

	if len(paths) != 2 || paths[0] != "/admin/applications" || paths[1] != "/admin-api/system/user/get" {
		t.Fatalf("下游路径 = %v，期望 [/admin/applications /admin-api/system/user/get]", paths)
	}
}

// TestCORSPreflightHandledByGateway 验证预检请求在入口直接返回，不打到下游，
// 且允许的方法与请求头回显浏览器声明。
func TestCORSPreflightHandledByGateway(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "yudao-system", BaseURL: upstream.URL}}
		cfg.LegacyRoutes = []config.LegacyRoute{{Prefix: "/admin-api/system", AppCode: "yudao-system"}}
	})

	response, body := env.do(t, requestOptions{
		method: http.MethodOptions,
		path:   "/admin-api/system/user/page",
		headers: map[string]string{
			"Origin":                         "http://localhost:5173",
			"Access-Control-Request-Method":  "POST",
			"Access-Control-Request-Headers": "content-type,authorization",
		},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("预检状态码 = %d，期望 200，响应 %s", response.StatusCode, body)
	}
	if got := response.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Errorf("Allow-Origin = %q，期望回显来源", got)
	}
	if got := response.Header.Get("Access-Control-Allow-Methods"); got != "POST" {
		t.Errorf("Allow-Methods = %q，期望回显 POST", got)
	}
	if got := response.Header.Get("Access-Control-Allow-Headers"); got != "content-type,authorization" {
		t.Errorf("Allow-Headers = %q，期望回显请求头", got)
	}
	if got := response.Header.Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Allow-Credentials = %q，期望 true", got)
	}
	if hits.Load() != 0 {
		t.Errorf("预检请求打到下游 %d 次，期望 0", hits.Load())
	}
}

// TestCORSAddedForSimpleRequest 验证普通跨域请求补响应头，
// 且下游已给出 CORS 头时不重复添加（避免浏览器判定失败）。
func TestCORSAddedForSimpleRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin-api/cors/downstream" {
			w.Header().Set("Access-Control-Allow-Origin", "http://downstream")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "yudao-system", BaseURL: upstream.URL}}
		cfg.LegacyRoutes = []config.LegacyRoute{
			{Prefix: "/admin-api/system", AppCode: "yudao-system"},
			{Prefix: "/admin-api/cors", AppCode: "yudao-system"},
		}
	})

	response, _ := env.do(t, requestOptions{
		path:    "/admin-api/system/user/page",
		headers: map[string]string{"Origin": "http://localhost:5173"},
	})
	if got := response.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Errorf("Allow-Origin = %q，期望回显来源", got)
	}

	// 下游已有 CORS 头时沿用下游值，不叠加成两个。
	response, _ = env.do(t, requestOptions{
		path:    "/admin-api/cors/downstream",
		headers: map[string]string{"Origin": "http://localhost:5173"},
	})
	if values := response.Header.Values("Access-Control-Allow-Origin"); len(values) != 1 || values[0] != "http://downstream" {
		t.Errorf("Allow-Origin = %v，期望只保留下游值", values)
	}
}

// TestTrustedProxyHeadersPreserved 验证 TRUST_PROXY_HEADERS=true 时，
// 上游代理写入的 X-Forwarded-For 不再被剥离，转发层再追加当前对端地址。
func TestTrustedProxyHeadersPreserved(t *testing.T) {
	var gotForwardedFor string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotForwardedFor = r.Header.Get(contracts.HeaderForwardedFor)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "yudao-system", BaseURL: upstream.URL}}
		cfg.LegacyRoutes = []config.LegacyRoute{{Prefix: "/admin-api/system", AppCode: "yudao-system"}}
		cfg.TrustProxyHeaders = true
	})

	response, _ := env.do(t, requestOptions{
		path:    "/admin-api/system/user/page",
		headers: map[string]string{contracts.HeaderForwardedFor: "203.0.113.9"},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", response.StatusCode)
	}
	if !strings.HasPrefix(gotForwardedFor, "203.0.113.9, ") {
		t.Errorf("X-Forwarded-For = %q，期望保留客户端来源并追加入口对端地址", gotForwardedFor)
	}
}

// TestWebSocketTunnel 验证 WebSocket 握手与 101 之后的双向字节流：
// 网关需要保留 Upgrade/Connection 等逐跳头，并在劫持连接后原样搬运数据。
func TestWebSocketTunnel(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动上游监听失败: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()

		br := bufio.NewReader(conn)
		request, readErr := http.ReadRequest(br)
		if readErr != nil {
			return
		}
		accept := webSocketAccept(request.Header.Get("Sec-WebSocket-Key"))
		_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: "+accept+"\r\n\r\n")
		// 模拟广播：把客户端发来的字节原样回显。
		_, _ = io.Copy(conn, br)
	}()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "yudao-infra", BaseURL: "http://" + listener.Addr().String()}}
		cfg.LegacyRoutes = []config.LegacyRoute{{Prefix: "/infra/ws", AppCode: "yudao-infra"}}
	})

	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(env.baseURL, "http://"), 3*time.Second)
	if err != nil {
		t.Fatalf("连接网关失败: %v", err)
	}
	defer conn.Close()

	_, _ = io.WriteString(conn, "GET /infra/ws?token=test HTTP/1.1\r\n"+
		"Host: "+strings.TrimPrefix(env.baseURL, "http://")+"\r\n"+
		"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")

	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("读取握手响应失败: %v", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("握手状态码 = %d，期望 101", response.StatusCode)
	}
	if got := response.Header.Get("Sec-WebSocket-Accept"); got != webSocketAccept("dGhlIHNhbXBsZSBub25jZQ==") {
		t.Errorf("Sec-WebSocket-Accept = %q，与上游不一致", got)
	}

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("写入隧道失败: %v", err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(reader, echo); err != nil {
		t.Fatalf("读取隧道回显失败: %v", err)
	}
	if string(echo) != "ping" {
		t.Errorf("隧道回显 = %q，期望 ping", echo)
	}
}

// TestSSEStreamOutlivesReadBudget 验证 AI SSE 不被读预算截断，且事件是实时下发的：
// 上游 250ms 后才发第二条事件，远晚于 80ms 的读预算；若预算仍覆盖响应体，
// 第二条事件会丢失，若 Hertz 缓冲则会表现为首条事件延迟到达。
func TestSSEStreamOutlivesReadBudget(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(250 * time.Millisecond)
		_, _ = io.WriteString(w, "data: second\n\n")
	}))
	defer upstream.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "yudao-ai", BaseURL: upstream.URL}}
		cfg.LegacyRoutes = []config.LegacyRoute{{Prefix: "/sse", AppCode: "yudao-ai"}}
		cfg.Timeouts.Read = 80 * time.Millisecond
		cfg.Timeouts.Stream = 0
		cfg.TimeoutRules.StreamPaths = []string{"/sse", "stream", "/events", "/mcp"}
	})

	response, err := env.client.Get(env.baseURL + "/sse")
	if err != nil {
		t.Fatalf("建立 SSE 连接失败: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", response.StatusCode)
	}
	if got := response.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q，期望 no", got)
	}

	start := time.Now()
	reader := bufio.NewReader(response.Body)
	first, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("读取首条事件失败: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("首条事件延迟 %s，疑似被网关缓冲", elapsed)
	}
	if !strings.Contains(first, "data: first") {
		t.Fatalf("首条事件 = %q", first)
	}

	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("读取后续事件失败: %v", err)
	}
	if !strings.Contains(string(rest), "data: second") {
		t.Fatalf("流被截断，后续内容 = %q", rest)
	}
}

// TestApiDocsRewrite 验证 /admin-api/{module}/v3/api-docs 会像 yudao-gateway
// 的 RewritePath 一样剥掉业务前缀，转发到服务根路径 /v3/api-docs。
func TestApiDocsRewrite(t *testing.T) {
	var paths, queries []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		queries = append(queries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"openapi":"3.1.0"}`))
	}))
	defer upstream.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{
			Code: "yudao-system", BaseURL: upstream.URL, UpstreamPath: "/admin-api",
		}}
		cfg.LegacyRoutes = []config.LegacyRoute{{Prefix: "/admin-api/system", AppCode: "yudao-system"}}
	})

	// 旧路径（yudao 前端/Knife4j 使用的形式）。
	if response, body := env.do(t, requestOptions{path: "/admin-api/system/v3/api-docs?group=legacy"}); response.StatusCode != http.StatusOK {
		t.Fatalf("旧路径文档状态码 = %d，响应 %s", response.StatusCode, body)
	}
	// 新路径族：先拼上 /admin-api/system，再统一重写。
	if response, body := env.do(t, requestOptions{path: "/admin/yudao-system/v1/system/v3/api-docs?group=family"}); response.StatusCode != http.StatusOK {
		t.Fatalf("新路径文档状态码 = %d，响应 %s", response.StatusCode, body)
	}
	// swagger-config 同样重写。
	if response, body := env.do(t, requestOptions{path: "/admin-api/system/v3/api-docs/swagger-config"}); response.StatusCode != http.StatusOK {
		t.Fatalf("swagger-config 状态码 = %d，响应 %s", response.StatusCode, body)
	}

	wantPaths := []string{"/v3/api-docs", "/v3/api-docs", "/v3/api-docs/swagger-config"}
	wantQueries := []string{"group=legacy", "group=family", ""}
	for i := range wantPaths {
		if paths[i] != wantPaths[i] {
			t.Errorf("第 %d 次下游路径 = %q，期望 %q", i+1, paths[i], wantPaths[i])
		}
		if queries[i] != wantQueries[i] {
			t.Errorf("第 %d 次查询串 = %q，期望 %q", i+1, queries[i], wantQueries[i])
		}
	}
}

// TestLegacyMCPMessageFallback 验证 yudao 的 /mcp/message 不会被
// /mcp/{app_code} 参数段路由吞掉，而是回落到旧路径映射。
func TestLegacyMCPMessageFallback(t *testing.T) {
	var gotPath, gotQuery, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		payload, _ := io.ReadAll(r.Body)
		gotBody = string(payload)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("accepted"))
	}))
	defer upstream.Close()

	env := startTestApp(t, func(cfg *config.Config) {
		cfg.Apps = []config.AppConfig{{Code: "yudao-ai", BaseURL: upstream.URL}}
		cfg.LegacyRoutes = []config.LegacyRoute{{Prefix: "/mcp/message", AppCode: "yudao-ai"}}
	})

	response, body := env.do(t, requestOptions{
		method: http.MethodPost,
		path:   "/mcp/message?sessionId=abc",
		body:   `{"jsonrpc":"2.0"}`,
	})
	if response.StatusCode != http.StatusAccepted || string(body) != "accepted" {
		t.Fatalf("状态码 = %d，响应 = %s", response.StatusCode, body)
	}
	if gotPath != "/mcp/message" || gotQuery != "sessionId=abc" || gotBody != `{"jsonrpc":"2.0"}` {
		t.Fatalf("下游收到 path=%q query=%q body=%q", gotPath, gotQuery, gotBody)
	}
}

// webSocketAccept 按 RFC 6455 计算握手响应头。
func webSocketAccept(key string) string {
	hash := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(hash[:])
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
			Read:        config.DefaultTimeoutRead,
			Write:       config.DefaultTimeoutWrite,
			Generate:    config.DefaultTimeoutGenerate,
			Upload:      config.DefaultTimeoutUpload,
			Static:      config.DefaultTimeoutStatic,
			MCP:         config.DefaultTimeoutMCP,
			Stream:      config.DefaultTimeoutStream,
			WSHandshake: config.DefaultWSHandshakeTimeout,
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

		Breaker: config.Breaker{
			Enabled:          config.DefaultBreakerEnabled,
			FailureThreshold: config.DefaultBreakerThreshold,
			Cooldown:         config.DefaultBreakerCooldown,
		},
		TimeoutRules: config.TimeoutRules{
			GeneratePaths: []string{"/agent", "/tasks", "/merge", "/previs", "/task-queue"},
			UploadPaths:   []string{"/upload"},
			StreamPaths:   []string{"/sse", "stream", "/events", "/mcp"},
		},
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
