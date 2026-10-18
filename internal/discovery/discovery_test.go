package discovery

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStaticResolver(t *testing.T) {
	resolver, err := NewStaticResolver(map[string][]string{
		"huobao": {"127.0.0.1:5679", "127.0.0.1:5680"},
	})
	if err != nil {
		t.Fatalf("构建静态解析器失败: %v", err)
	}

	instances, err := resolver.Resolve(context.Background(), "huobao")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(instances) != 2 {
		t.Fatalf("实例数 = %d，期望 2", len(instances))
	}
	if instances[0].Address() != "127.0.0.1:5679" || instances[0].Weight != 1 {
		t.Errorf("实例信息异常: %+v", instances[0])
	}
	if !instances[0].Usable() {
		t.Error("静态实例应默认可用于负载均衡")
	}

	// 返回值为内部缓存的直接引用（零拷贝），读取行为必须稳定。
	again, _ := resolver.Resolve(context.Background(), "huobao")
	if len(again) != 2 || again[0].Host != "127.0.0.1" {
		t.Error("重复解析应得到一致结果")
	}

	if _, err := resolver.Resolve(context.Background(), "unknown"); err == nil {
		t.Error("未配置的服务应报错")
	}
}

func TestStaticResolverRejectsBadAddress(t *testing.T) {
	if _, err := NewStaticResolver(map[string][]string{"huobao": {"127.0.0.1"}}); err == nil {
		t.Error("缺少端口的地址应报错")
	}
	if _, err := NewStaticResolver(map[string][]string{"huobao": {"127.0.0.1:abc"}}); err == nil {
		t.Error("端口非数字应报错")
	}
}

func TestStaticResolverPreservesSchemeAndDefaultPort(t *testing.T) {
	cases := []struct {
		address string
		want    string
	}{
		{"https://example.com", "https://example.com:443"},
		{"http://example.com", "http://example.com:80"},
		{"https://example.com:8443/api", "https://example.com:8443"},
		{"https://[::1]", "https://[::1]:443"},
		{"127.0.0.1:8080", "http://127.0.0.1:8080"},
	}
	for _, tc := range cases {
		t.Run(tc.address, func(t *testing.T) {
			resolver, err := NewStaticResolver(map[string][]string{"system": {tc.address}})
			if err != nil {
				t.Fatal(err)
			}
			instances, err := resolver.Resolve(context.Background(), "system")
			if err != nil || len(instances) != 1 {
				t.Fatalf("解析失败: %v, %v", instances, err)
			}
			if got := instances[0].URL(); got != tc.want {
				t.Errorf("URL = %q，期望 %q", got, tc.want)
			}
		})
	}
	for _, address := range []string{"ftp://example.com:21", "https://:443", "http://example.com:0"} {
		if _, err := NewStaticResolver(map[string][]string{"system": {address}}); err == nil {
			t.Errorf("应拒绝非法地址 %q", address)
		}
	}
}

func TestMultiResolverMapsAppCodeToServiceName(t *testing.T) {
	static, err := NewStaticResolver(map[string][]string{"yudao-system": {"127.0.0.1:48081"}})
	if err != nil {
		t.Fatal(err)
	}
	multi := NewMulti()
	multi.AddAlias("system", "yudao-system", static)
	instances, err := multi.Resolve(context.Background(), "system")
	if err != nil || len(instances) != 1 || instances[0].Service != "yudao-system" {
		t.Fatalf("别名解析失败: %v, %v", instances, err)
	}
	if services := multi.Services(); len(services) != 1 || services[0] != "system" {
		t.Fatalf("应用编码列表错误: %v", services)
	}
}

func TestMultiResolver(t *testing.T) {
	static, err := NewStaticResolver(map[string][]string{"huobao": {"127.0.0.1:5679"}})
	if err != nil {
		t.Fatalf("构建静态解析器失败: %v", err)
	}

	multi := NewMulti()
	multi.Add("huobao", static)

	instances, err := multi.Resolve(context.Background(), "huobao")
	if err != nil || len(instances) != 1 {
		t.Fatalf("解析 huobao 失败: %v, 实例数 %d", err, len(instances))
	}
	if _, err := multi.Resolve(context.Background(), "other"); err == nil {
		t.Error("未配置解析来源的服务应报错")
	}
	if len(multi.Services()) != 1 {
		t.Errorf("Services = %v", multi.Services())
	}
}

// newNacosServer 返回一个模拟 Nacos 开放 API 的服务器，并记录最后一次请求的查询串。
func newNacosServer(t *testing.T, hosts []map[string]any, captured *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if captured != nil {
			*captured = r.URL.RawQuery
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"hosts": hosts})
	}))
}

func TestNacosResolverParsesInstances(t *testing.T) {
	var query string
	server := newNacosServer(t, []map[string]any{
		{"ip": "10.0.0.1", "port": 8888, "weight": 2.0, "healthy": true, "enabled": true,
			"metadata": map[string]string{"version": "1.0", "tag": ""}},
		{"ip": "10.0.0.2", "port": 8889, "weight": 0, "healthy": true, "enabled": true,
			"metadata": map[string]string{}},
		// 不健康实例必须被过滤掉，不能进入负载均衡。
		{"ip": "10.0.0.3", "port": 8890, "healthy": false, "enabled": true},
		// 已禁用实例同样必须被过滤。
		{"ip": "10.0.0.4", "port": 8891, "healthy": true, "enabled": false},
	}, &query)
	defer server.Close()

	addr := strings.TrimPrefix(server.URL, "http://")
	resolver, err := NewNacosResolver(NacosConfig{Addr: addr, Group: "MY_GROUP", Namespace: "ns-1", Refresh: time.Minute})
	if err != nil {
		t.Fatalf("构建 Nacos 解析器失败: %v", err)
	}

	instances, err := resolver.Resolve(context.Background(), "yudao-system")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// 解析器原样返回注册中心数据（含不健康实例），由负载均衡按 Usable() 过滤——
	// 这样 /readyz 才能看到真实实例数，而不是被悄悄隐藏的故障。
	if len(instances) != 4 {
		t.Fatalf("实例数 = %d，期望 4", len(instances))
	}

	usable := 0
	for _, item := range instances {
		if item.Usable() {
			usable++
		}
	}
	if usable != 2 {
		t.Errorf("可用实例数 = %d，期望 2（不健康与已禁用应被负载均衡过滤）", usable)
	}

	first := instances[0]
	if first.Host != "10.0.0.1" || first.Port != 8888 || first.Weight != 2 {
		t.Errorf("实例解析异常: %+v", first)
	}
	if first.Version() != "1.0" {
		t.Errorf("灰度版本未解析: %q", first.Version())
	}

	// weight 为 0 时应归一为 1，避免权重全零导致 LB 退化行为不确定。
	if instances[1].Weight != 1 {
		t.Errorf("weight 应归一为 1，实际 %v", instances[1].Weight)
	}

	for _, param := range []string{"serviceName=yudao-system", "groupName=MY_GROUP", "namespaceId=ns-1"} {
		if !strings.Contains(query, param) {
			t.Errorf("请求缺少参数 %s，实际 %q", param, query)
		}
	}
}

func TestNacosResolverFallsBackToStaleCache(t *testing.T) {
	var fail atomic.Bool
	var requests, failures atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if fail.Load() {
			failures.Add(1)
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"forbidden"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"hosts": []map[string]any{
			{"ip": "10.0.0.1", "port": 8888, "healthy": true, "enabled": true},
		}})
	}))
	defer server.Close()

	resolver, err := NewNacosResolver(NacosConfig{Addr: strings.TrimPrefix(server.URL, "http://"), Refresh: time.Millisecond})
	if err != nil {
		t.Fatalf("构建解析器失败: %v", err)
	}

	if _, err := resolver.Resolve(context.Background(), "svc"); err != nil {
		t.Fatalf("首次解析应成功: %v", err)
	}

	resolver.mu.Lock()
	entry := resolver.cache["svc"]
	entry.fetchedAt = time.Now().Add(-time.Hour)
	resolver.cache["svc"] = entry
	resolver.mu.Unlock()
	fail.Store(true)
	instances, err := resolver.Resolve(context.Background(), "svc")
	if err != nil {
		t.Fatalf("上游故障时应降级使用缓存，实际报错: %v", err)
	}
	if len(instances) != 1 || instances[0].Host != "10.0.0.1" {
		t.Errorf("降级缓存内容异常: %+v", instances)
	}
	if requests.Load() != 2 || failures.Load() != 1 {
		t.Fatalf("未验证真实失败拉取: requests=%d failures=%d", requests.Load(), failures.Load())
	}
	fail.Store(false)
	if _, err := resolver.Resolve(context.Background(), "svc"); err != nil {
		t.Fatalf("上游恢复后重新拉取失败: %v", err)
	}
	if requests.Load() != 3 {
		t.Error("失败拉取不应刷新缓存时间，上游恢复后必须重新拉取")
	}
}

func TestNacosResolverFailsWhenNoCache(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "sensitive-nacos-error-body")
	}))
	defer server.Close()

	resolver, err := NewNacosResolver(NacosConfig{Addr: strings.TrimPrefix(server.URL, "http://"), Refresh: time.Minute})
	if err != nil {
		t.Fatalf("构建解析器失败: %v", err)
	}

	if _, err := resolver.Resolve(context.Background(), "svc"); err == nil {
		t.Fatal("无缓存且上游失败时必须报错，否则调用方无法映射 50201")
	} else if strings.Contains(err.Error(), "sensitive-nacos-error-body") {
		t.Error("发现错误中不得包含 Nacos 响应正文")
	}
}

func TestNacosRefreshLogsExcludeErrorBody(t *testing.T) {
	const secret = "private-nacos-error-body"
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, secret)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"hosts": []any{}})
	}))
	defer server.Close()
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	resolver, err := NewNacosResolver(NacosConfig{
		Addr: strings.TrimPrefix(server.URL, "http://"), Refresh: 10 * time.Millisecond,
		Logger: slog.New(slog.NewJSONHandler(writer, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(context.Background(), "svc"); err != nil {
		t.Fatal(err)
	}
	entries := make(chan map[string]any, 1)
	go func() {
		var entry map[string]any
		_ = json.NewDecoder(reader).Decode(&entry)
		entries <- entry
	}()
	fail.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resolver.Start(ctx)
	select {
	case entry := <-entries:
		encoded, _ := json.Marshal(entry)
		if strings.Contains(string(encoded), secret) || entry["err"] != nil {
			t.Fatal("后台刷新日志不得包含原始错误或响应正文")
		}
		if entry["error_type"] == nil || entry["level"] != "WARN" {
			t.Fatalf("缺少安全诊断字段: %s", encoded)
		}
	case <-time.After(time.Second):
		t.Fatal("未观察到真实失败拉取的告警日志")
	}
}

func TestNacosResolverRequiresAddr(t *testing.T) {
	if _, err := NewNacosResolver(NacosConfig{}); err == nil {
		t.Error("缺少 Nacos 地址应报错")
	}
	if _, err := NewNacosResolver(NacosConfig{Addr: "127.0.0.1:8848", Refresh: -time.Second}); err == nil {
		t.Error("负刷新周期应报错")
	}
}

func TestNacosRefreshUpdatesCache(t *testing.T) {
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"hosts": []map[string]any{
			{"ip": "10.0.0.9", "port": 8888, "healthy": true, "enabled": true},
		}})
	}))
	defer server.Close()

	resolver, err := NewNacosResolver(NacosConfig{Addr: strings.TrimPrefix(server.URL, "http://"), Refresh: time.Millisecond})
	if err != nil {
		t.Fatalf("构建解析器失败: %v", err)
	}
	if _, err := resolver.Resolve(context.Background(), "svc"); err != nil {
		t.Fatalf("首次解析失败: %v", err)
	}

	// 后台刷新失败时 Refresh 返回错误，但缓存仍可用。
	fail.Store(true)
	if err := resolver.Refresh(context.Background()); err == nil {
		t.Error("刷新失败应返回错误")
	}
	instances, err := resolver.Resolve(context.Background(), "svc")
	if err != nil || len(instances) != 1 {
		t.Errorf("刷新失败后仍应可用缓存: %v %+v", err, instances)
	}
}
