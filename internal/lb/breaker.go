package lb

import (
	"sync"
	"time"

	"newx-gateway/internal/discovery"
)

// Breaker 是按实例的失败熔断器。
//
// 为什么需要它：注册中心（Nacos）有健康检查，坏实例会被摘掉；但静态地址场景下
// 没有任何健康信号，一个已经宕掉的实例会被持续选中，每次都白等一次拨号超时。
// 熔断器用「连续失败计数 + 冷却期」把这类实例临时挪出候选集。
//
// 语义刻意保持简单：
//   - 连续失败达到阈值 → 进入冷却期，期间不参与选取；
//   - 冷却结束自动进入半开（放一个请求试探），成功即复位，失败则重新冷却；
//   - 任何一次成功都立即清零失败计数。
type Breaker struct {
	mu        sync.Mutex
	threshold int
	cooldown  time.Duration
	now       func() time.Time

	// failures 是连续失败次数，key 为实例地址。
	failures map[string]int
	// openUntil 是熔断截止时间，key 为实例地址；存在即处于打开状态。
	openUntil map[string]time.Time
}

// NewBreaker 构建熔断器。threshold 与 cooldown 必须为正数。
func NewBreaker(threshold int, cooldown time.Duration) *Breaker {
	if threshold <= 0 {
		threshold = 1
	}
	if cooldown <= 0 {
		cooldown = time.Second
	}
	return &Breaker{
		threshold: threshold,
		cooldown:  cooldown,
		now:       time.Now,
		failures:  make(map[string]int),
		openUntil: make(map[string]time.Time),
	}
}

// Allow 报告该实例当前是否可被选取。
func (b *Breaker) Allow(address string) bool {
	if b == nil || address == "" {
		return true
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	until, open := b.openUntil[address]
	if !open {
		return true
	}
	// 冷却期结束：允许试探性放行（半开），由调用方的结果决定复位或续熔。
	if !b.now().Before(until) {
		return true
	}
	return false
}

// RecordSuccess 复位该实例的失败计数并解除熔断。
func (b *Breaker) RecordSuccess(address string) {
	if b == nil || address == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.failures, address)
	delete(b.openUntil, address)
}

// RecordFailure 记录一次失败；达到阈值则打开熔断。
func (b *Breaker) RecordFailure(address string) {
	if b == nil || address == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	b.failures[address]++
	if b.failures[address] >= b.threshold {
		b.openUntil[address] = b.now().Add(b.cooldown)
		// 复位计数，让冷却结束后从零开始重新累计。
		b.failures[address] = 0
	}
}

// State 返回该实例的熔断状态，用于自检与测试。
func (b *Breaker) State(address string) (open bool, failures int) {
	if b == nil {
		return false, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	until, isOpen := b.openUntil[address]
	if isOpen && b.now().Before(until) {
		return true, b.failures[address]
	}
	return false, b.failures[address]
}

// Available 过滤掉处于熔断状态的实例。
func (b *Breaker) Available(instances []discovery.Instance) []discovery.Instance {
	if b == nil {
		return instances
	}
	result := make([]discovery.Instance, 0, len(instances))
	for _, instance := range instances {
		if b.Allow(instance.Address()) {
			result = append(result, instance)
		}
	}
	return result
}

// Reset 清空全部状态，便于测试。
func (b *Breaker) Reset() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = make(map[string]int)
	b.openUntil = make(map[string]time.Time)
}
