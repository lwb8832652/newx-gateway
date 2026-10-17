package middleware

import (
	"context"
	"sort"
	"time"

	"github.com/cloudwego/hertz/pkg/app"

	"newx-gateway/internal/config"
	"newx-gateway/internal/contracts"
)

// 分桶名称。与路由家族对应，接入计数逻辑时按此维度统计。
const (
	BucketWeb          = "web"
	BucketWebGenerate  = "web.generate"
	BucketWebUpload    = "web.upload"
	BucketAdmin        = "admin"
	BucketMCP          = "mcp"
	BucketOpenAPI      = "openapi"
	BucketStaticFamily = "static"
)

// Bucket 是一个限流分桶的定义。
type Bucket struct {
	Name   string
	Limit  int
	Window time.Duration
}

// Limiter 是限流中间件的空位实现。
//
// 本阶段只把配置固化为分桶定义，不做计数与拦截：
// 限流阈值需要结合真实压力基线确定，过早拦截会掩盖容量问题。
// 接入方式（后续）：按主体（user_id / credential / client_id）在内存或 Redis 计数，
// 超限返回 42901 并设置 Retry-After；生成类额外受 MaxConcurrencyGenerate 约束，
// 超并发时快速失败 50201，不在入口排队。
type Limiter struct {
	enabled bool
	buckets map[string]Bucket
	order   []string
}

// NewLimiter 依据配置构造分桶定义。
func NewLimiter(limits config.RateLimits, mcpEnabled, openAPIEnabled bool) *Limiter {
	definitions := []Bucket{
		{Name: BucketWeb, Limit: limits.Web, Window: time.Minute},
		{Name: BucketWebGenerate, Limit: limits.WebGenerate, Window: time.Minute},
		{Name: BucketAdmin, Limit: limits.Admin, Window: time.Minute},
	}
	if mcpEnabled {
		definitions = append(definitions, Bucket{Name: BucketMCP, Limit: limits.MCP, Window: time.Minute})
	}
	if openAPIEnabled {
		// OpenAPI 默认关闭；开启时先复用 admin 阈值，正式配额后续独立配置。
		definitions = append(definitions, Bucket{Name: BucketOpenAPI, Limit: limits.Admin, Window: time.Minute})
	}

	limiter := &Limiter{buckets: make(map[string]Bucket, len(definitions))}
	for _, bucket := range definitions {
		if bucket.Limit <= 0 {
			continue
		}
		limiter.buckets[bucket.Name] = bucket
		limiter.order = append(limiter.order, bucket.Name)
	}
	sort.Strings(limiter.order)
	return limiter
}

// SetEnabled 开关限流。
func (l *Limiter) SetEnabled(enabled bool) {
	if l != nil {
		l.enabled = enabled
	}
}

// Enabled 报告限流是否已生效。
func (l *Limiter) Enabled() bool { return l != nil && l.enabled }

// Buckets 返回全部分桶定义，按名称排序，便于测试与自检输出。
func (l *Limiter) Buckets() []Bucket {
	if l == nil {
		return nil
	}
	result := make([]Bucket, 0, len(l.order))
	for _, name := range l.order {
		result = append(result, l.buckets[name])
	}
	return result
}

// BucketFor 返回指定分桶的定义。
func (l *Limiter) BucketFor(name string) (Bucket, bool) {
	if l == nil {
		return Bucket{}, false
	}
	bucket, ok := l.buckets[name]
	return bucket, ok
}

// Middleware 返回指定分桶的限流中间件。
// 未启用时恒放行，仅登记本次请求命中的分桶，供日志归类。
func (l *Limiter) Middleware(bucketName string) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		if bucketName != "" {
			c.Set(contracts.CtxRateBucket, bucketName)
		}
		// TODO(限流): 启用后在主体维度做计数，超限返回 contracts.CodeRateLimited 并设置 Retry-After。
		c.Next(ctx)
	}
}
