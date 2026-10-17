package middleware

import (
	"testing"
	"time"

	"newx-gateway/internal/config"
)

func testRates() config.RateLimits {
	return config.RateLimits{Web: 120, WebGenerate: 10, Admin: 300, MCP: 60, MaxConcurrencyGenerate: 32}
}

func TestNewLimiterBuckets(t *testing.T) {
	limiter := NewLimiter(testRates(), true, false)

	buckets := limiter.Buckets()
	if len(buckets) != 4 {
		t.Fatalf("应生成 4 个分桶（含 mcp，不含关闭的 openapi），实际 %d: %+v", len(buckets), buckets)
	}

	// 分桶按名称排序，便于自检输出稳定。
	for i := 1; i < len(buckets); i++ {
		if buckets[i-1].Name > buckets[i].Name {
			t.Errorf("分桶未按名称排序: %+v", buckets)
			break
		}
	}

	web, ok := limiter.BucketFor(BucketWeb)
	if !ok || web.Limit != 120 || web.Window != time.Minute {
		t.Errorf("web 分桶异常: %+v, ok=%v", web, ok)
	}
	if _, ok := limiter.BucketFor(BucketOpenAPI); ok {
		t.Error("OpenAPI 关闭时不应创建分桶")
	}
}

func TestNewLimiterSkipsNonPositiveLimits(t *testing.T) {
	limiter := NewLimiter(config.RateLimits{Web: 0, Admin: -1, MCP: 10}, true, false)
	buckets := limiter.Buckets()
	if len(buckets) != 1 || buckets[0].Name != BucketMCP {
		t.Errorf("非正数阈值应被跳过，实际 %+v", buckets)
	}
}

func TestLimiterDisabledByDefault(t *testing.T) {
	limiter := NewLimiter(testRates(), true, false)
	if limiter.Enabled() {
		t.Error("默认必须关闭限流：阈值需结合真实压力基线确定")
	}
	limiter.SetEnabled(true)
	if !limiter.Enabled() {
		t.Error("SetEnabled(true) 未生效")
	}
}

func TestOpenAPIBucketUsesAdminLimitPlaceholder(t *testing.T) {
	limiter := NewLimiter(testRates(), true, true)
	bucket, ok := limiter.BucketFor(BucketOpenAPI)
	if !ok {
		t.Fatal("OpenAPI 开启时应创建分桶")
	}
	if bucket.Limit != testRates().Admin {
		t.Errorf("OpenAPI 暂用 admin 阈值占位，实际 %d", bucket.Limit)
	}
}

func TestNilLimiterIsSafe(t *testing.T) {
	var limiter *Limiter
	if limiter.Enabled() {
		t.Error("nil 限流器不应报告已启用")
	}
	if limiter.Buckets() != nil {
		t.Error("nil 限流器应返回 nil 分桶")
	}
	if _, ok := limiter.BucketFor(BucketWeb); ok {
		t.Error("nil 限流器不应命中分桶")
	}
}
