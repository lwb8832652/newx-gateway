package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoginUserHeaderValueRoundTrip(t *testing.T) {
	user := &LoginUser{
		ID:       123,
		UserType: 1,
		Info:     map[string]string{"nickname": "管理员"},
		TenantID: 10,
		Scopes:   []string{"system:user:query"},
		// yudao 的自定义 Jackson 模块在传输时使用毫秒时间戳。
		ExpiresTime: time.Date(2026, 10, 17, 14, 30, 0, 0, time.Local),
	}

	header, err := user.HeaderValue()
	if err != nil {
		t.Fatalf("生成 login-user 头失败: %v", err)
	}

	// 必须经过 URL-encode，否则中文与 JSON 特殊字符会破坏请求头。
	if header == "" || header[0] == '{' {
		t.Errorf("login-user 头应被 URL-encode，实际 %q", header)
	}

	parsed, err := ParseLoginUser(header)
	if err != nil {
		t.Fatalf("解析 login-user 头失败: %v", err)
	}
	if parsed.ID != user.ID || parsed.TenantID != user.TenantID || parsed.UserType != user.UserType {
		t.Errorf("往返后字段丢失: %+v", parsed)
	}
	if parsed.Info["nickname"] != "管理员" {
		t.Errorf("info 未保留: %+v", parsed.Info)
	}
	if len(parsed.Scopes) != 1 || parsed.Scopes[0] != "system:user:query" {
		t.Errorf("scopes 未保留: %+v", parsed.Scopes)
	}

	// 时间必须按 yudao 的格式输出，且反序列化后语义不变。
	if FormatExpiresTime(parsed.ExpiresTime) != "2026-10-17T14:30:00" {
		t.Errorf("过期时间格式 = %q，期望 2026-10-17T14:30:00", FormatExpiresTime(parsed.ExpiresTime))
	}
}

func TestLoginUserNormalizesZeroValues(t *testing.T) {
	header, err := (&LoginUser{ID: 1}).HeaderValue()
	if err != nil {
		t.Fatalf("生成头失败: %v", err)
	}
	parsed, err := ParseLoginUser(header)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if parsed.Info == nil || parsed.Scopes == nil {
		t.Errorf("零值字段应归一为空集合，实际 info=%v scopes=%v", parsed.Info, parsed.Scopes)
	}
}

func TestLoginUserEmptyAndExpired(t *testing.T) {
	var nilUser *LoginUser
	if !nilUser.Empty() {
		t.Error("nil 用户应视为空")
	}
	if !nilUser.Expired(time.Now()) {
		t.Error("nil 用户应视为已过期")
	}
	if !(&LoginUser{}).Empty() {
		t.Error("ID 为 0 应视为空用户")
	}

	future := &LoginUser{ID: 1, ExpiresTime: time.Now().Add(time.Minute)}
	if future.Empty() || future.Expired(time.Now()) {
		t.Error("未过期用户不应判空/判过期")
	}

	past := &LoginUser{ID: 1, ExpiresTime: time.Now().Add(-time.Minute)}
	if !past.Expired(time.Now()) {
		t.Error("已过期的用户应被识别")
	}
}

// newCheckServer 返回一个模拟 yudao check-token 接口的服务器。
func newCheckServer(t *testing.T, respond func(accessToken string) (int, any)) (*httptest.Server, *int64) {
	t.Helper()
	var tenantSeen int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accessToken := r.URL.Query().Get("accessToken")
		if tenant := r.Header.Get("tenant-id"); tenant != "" {
			value, _ := parseInt64(tenant)
			tenantSeen = value
		}
		code, payload := respond(accessToken)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(server.Close)
	return server, &tenantSeen
}

func parseInt64(raw string) (int64, error) {
	var value int64
	err := json.Unmarshal([]byte(raw), &value)
	return value, err
}

func validPayload() map[string]any {
	return map[string]any{
		"code": 0,
		"data": map[string]any{
			"userId":      42,
			"userType":    1,
			"userInfo":    map[string]string{"nickname": "测试"},
			"tenantId":    7,
			"scopes":      []string{"system:user:query"},
			"expiresTime": "2099-01-01T00:00:00",
		},
		"message": "",
	}
}

func TestTokenClientCheckSuccess(t *testing.T) {
	server, tenantSeen := newCheckServer(t, func(accessToken string) (int, any) {
		if accessToken != "token-abc" {
			t.Errorf("accessToken = %q，期望 token-abc", accessToken)
		}
		return http.StatusOK, validPayload()
	})

	client := NewTokenClient(func(context.Context) (string, error) { return server.URL, nil }, "", time.Second)
	user, err := client.Check(context.Background(), 7, "token-abc")
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if user == nil || user.ID != 42 || user.TenantID != 7 {
		t.Fatalf("用户信息异常: %+v", user)
	}
	if *tenantSeen != 7 {
		t.Errorf("tenant-id 头未随校验请求送出，实际 %d", *tenantSeen)
	}
	if len(user.Scopes) != 1 || user.Scopes[0] != "system:user:query" {
		t.Errorf("scopes 异常: %+v", user.Scopes)
	}
	if user.Empty() || user.Expired(time.Now()) {
		t.Error("解析出的用户应为有效未过期")
	}
}

func TestTokenClientInvalidTokenReturnsNilUser(t *testing.T) {
	server, _ := newCheckServer(t, func(accessToken string) (int, any) {
		return http.StatusOK, map[string]any{"code": 401, "data": nil, "message": "令牌已过期"}
	})

	client := NewTokenClient(func(context.Context) (string, error) { return server.URL, nil }, "", time.Second)
	// 令牌无效不是错误：按无身份处理，由下游判断是否需要登录。
	user, err := client.Check(context.Background(), 0, "expired-token")
	if err != nil {
		t.Fatalf("令牌无效不应返回错误: %v", err)
	}
	if !user.Empty() {
		t.Errorf("应返回空用户，实际 %+v", user)
	}
}

func TestTokenClientBusinessError(t *testing.T) {
	server, _ := newCheckServer(t, func(accessToken string) (int, any) {
		return http.StatusOK, map[string]any{"code": 500, "data": nil, "message": "系统异常"}
	})

	client := NewTokenClient(func(context.Context) (string, error) { return server.URL, nil }, "", time.Second)
	if _, err := client.Check(context.Background(), 0, "t"); err == nil {
		t.Fatal("非 401 的业务错误应返回错误")
	}
}

func TestTokenClientTransportError(t *testing.T) {
	client := NewTokenClient(func(context.Context) (string, error) {
		return "", context.DeadlineExceeded
	}, "", time.Second)
	if _, err := client.Check(context.Background(), 0, "t"); err == nil {
		t.Fatal("拨号失败应返回错误，由调用方决定放行策略")
	}
}

func TestTokenClientDefaultPath(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"data":null}`))
	}))
	defer server.Close()

	client := NewTokenClient(func(context.Context) (string, error) { return server.URL, nil }, "", time.Second)
	if _, err := client.Check(context.Background(), 0, "t"); err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if gotPath != checkTokenPath {
		t.Errorf("默认校验路径 = %q，期望 %q", gotPath, checkTokenPath)
	}
}

func TestTokenClientNilIsSafe(t *testing.T) {
	var client *TokenClient
	if _, err := client.Check(context.Background(), 0, "t"); err == nil {
		t.Error("nil 客户端应返回错误")
	}
}

func TestCacheHitAndExpiry(t *testing.T) {
	clock := time.Now()
	cache := newLoginUserCache(time.Minute)
	cache.now = func() time.Time { return clock }

	key := cacheKey(7, "token")
	user := &LoginUser{ID: 42}

	cache.Put(key, user)
	if got, ok := cache.Get(key); !ok || got.ID != 42 {
		t.Fatalf("缓存应命中，实际 %+v ok=%v", got, ok)
	}

	// 过期后必须失效，让下一次请求重新校验。
	clock = clock.Add(90 * time.Second)
	if _, ok := cache.Get(key); ok {
		t.Error("过期缓存不应命中")
	}
}

func TestCacheNegativeCaching(t *testing.T) {
	cache := newLoginUserCache(time.Minute)
	key := cacheKey(0, "bad-token")

	// 负缓存：存 nil 表示「无身份」。
	cache.Put(key, nil)
	user, ok := cache.Get(key)
	if !ok {
		t.Fatal("负缓存应命中")
	}
	if !user.Empty() {
		t.Errorf("负缓存应返回空用户，实际 %+v", user)
	}
}

func TestCacheDoCoalescesConcurrentLoads(t *testing.T) {
	var calls int32
	cache := newLoginUserCache(time.Minute)

	loader := func(context.Context) (*LoginUser, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(20 * time.Millisecond)
		return &LoginUser{ID: 1}, nil
	}

	done := make(chan struct{}, 8)
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			if _, err := cache.Do(context.Background(), "k", loader); err != nil {
				t.Errorf("加载失败: %v", err)
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("并发加载被合并为 %d 次远程调用，期望 1", got)
	}
}

func TestCacheDoDoesNotCacheErrors(t *testing.T) {
	cache := newLoginUserCache(time.Minute)
	attempts := 0

	loader := func(context.Context) (*LoginUser, error) {
		attempts++
		return nil, context.DeadlineExceeded
	}

	for i := 0; i < 2; i++ {
		if _, err := cache.Do(context.Background(), "k", loader); err == nil {
			t.Fatal("应返回错误")
		}
	}
	if attempts != 2 {
		t.Errorf("失败不应写入缓存，实际调用 %d 次", attempts)
	}
	if cache.Len() != 0 {
		t.Errorf("缓存应保持为空，实际 %d 条", cache.Len())
	}
}

func TestCacheKeyIncludesTenant(t *testing.T) {
	if cacheKey(1, "t") == cacheKey(2, "t") || cacheKey(-1, "t") == cacheKey(0, "t") {
		t.Error("缓存键必须区分租户及缺失与显式零租户")
	}
}

func TestCacheSweepsExpiredUnvisitedEntries(t *testing.T) {
	clock := time.Now()
	cache := newLoginUserCache(time.Minute)
	cache.now = func() time.Time { return clock }
	cache.Put("old-positive", &LoginUser{ID: 1})
	cache.Put("old-negative", nil)
	clock = clock.Add(time.Minute)
	cache.Put("new", &LoginUser{ID: 2})
	if cache.Len() != 1 {
		t.Fatalf("expired unvisited entries retained: %d", cache.Len())
	}
	if user, ok := cache.Get("new"); !ok || user.ID != 2 {
		t.Fatalf("new entry missing: %+v %v", user, ok)
	}
}

func TestCacheCapacityBoundsPositiveAndNegativeEntries(t *testing.T) {
	cache := newLoginUserCache(time.Minute)
	if cache.maxEntries != defaultLoginUserCacheCapacity || cache.maxEntries <= 0 {
		t.Fatalf("invalid default capacity: %d", cache.maxEntries)
	}
	cache.maxEntries = 3
	for i := int64(0); i < 100; i++ {
		var user *LoginUser
		if i%2 == 0 {
			user = &LoginUser{ID: i + 1}
		}
		key := cacheKey(i, "random-token")
		cache.Put(key, user)
		if cache.Len() > 3 {
			t.Fatalf("cache exceeded capacity: %d", cache.Len())
		}
		if got, ok := cache.Get(key); !ok || got != user {
			t.Fatalf("latest entry missing: %+v %v", got, ok)
		}
	}
	cache.Put(cacheKey(99, "random-token"), &LoginUser{ID: 999})
	if cache.Len() != 3 {
		t.Fatalf("updating existing entry should not evict: %d", cache.Len())
	}
}
