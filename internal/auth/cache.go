package auth

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// loginUserCache 是按 (tenantId, accessToken) 的登录用户缓存。
//
// 与 yudao 的 Guava 缓存行为对齐：
//   - TTL 固定 1 分钟（可配置），到期后由下一次请求触发刷新；
//   - 支持负缓存：令牌无效时缓存「空用户」，避免同一个失效令牌
//     在 TTL 内反复打 check-token；
//   - 合并并发请求：同一个 key 同时只发出一次远程调用，
//     避免热点令牌在缓存失效瞬间把鉴权方打满。
type loginUserCache struct {
	ttl time.Duration
	now func() time.Time

	mu       sync.Mutex
	entries  map[string]*cacheEntry
	inflight map[string]*inflightCall
}

type cacheEntry struct {
	user      *LoginUser
	expiresAt time.Time
}

type inflightCall struct {
	done chan struct{}
	user *LoginUser
	err  error
}

func newLoginUserCache(ttl time.Duration) *loginUserCache {
	if ttl <= 0 {
		ttl = time.Minute
	}
	return &loginUserCache{
		ttl:      ttl,
		now:      time.Now,
		entries:  make(map[string]*cacheEntry),
		inflight: make(map[string]*inflightCall),
	}
}

// Get 返回缓存的用户。第二个返回值表示是否命中。
func (c *loginUserCache) Get(key string) (*LoginUser, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if !c.now().Before(entry.expiresAt) {
		delete(c.entries, key)
		return nil, false
	}
	return entry.user, true
}

// Put 写入缓存（含负缓存：user 为 nil 表示无身份）。
func (c *loginUserCache) Put(key string, user *LoginUser) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = &cacheEntry{user: user, expiresAt: c.now().Add(c.ttl)}
}

// Do 执行加载：命中缓存直接返回，否则合并并发调用执行 loader。
func (c *loginUserCache) Do(ctx context.Context, key string, loader func(context.Context) (*LoginUser, error)) (*LoginUser, error) {
	if user, ok := c.Get(key); ok {
		return user, nil
	}

	c.mu.Lock()
	if call, waiting := c.inflight[key]; waiting {
		// 已有请求在飞：等它完成，复用结果。
		c.mu.Unlock()
		select {
		case <-call.done:
			return call.user, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	call := &inflightCall{done: make(chan struct{})}
	c.inflight[key] = call
	c.mu.Unlock()

	// loader  panic 会让等待方永久阻塞，用 defer 保证信号一定发出。
	defer func() {
		c.mu.Lock()
		delete(c.inflight, key)
		c.mu.Unlock()
		close(call.done)
	}()

	user, err := loader(ctx)
	call.user, call.err = user, err
	if err == nil {
		// 失败不写缓存：下次请求重试，避免一次抖动把用户锁在门外一整个 TTL。
		c.Put(key, user)
	}
	return user, err
}

// Len 返回当前缓存条目数，供自检与测试使用。
func (c *loginUserCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// cacheKey 拼装缓存键：租户 + 令牌，与 yudao 的 KeyValue<Long, String> 一致。
func cacheKey(tenantID int64, accessToken string) string {
	return strconv.FormatInt(tenantID, 10) + "|" + accessToken
}
