package lb

import (
	"testing"
	"time"

	"newx-gateway/internal/discovery"
)

// fakeClock 让熔断测试不依赖真实时间。
type fakeClock struct {
	now time.Time
}

func (f *fakeClock) Now() time.Time { return f.now }

func newTestBreaker(threshold int, cooldown time.Duration) (*Breaker, *fakeClock) {
	clock := &fakeClock{now: time.Unix(1700000000, 0)}
	breaker := NewBreaker(threshold, cooldown)
	breaker.now = clock.Now
	return breaker, clock
}

func TestBreakerOpensAfterThreshold(t *testing.T) {
	breaker, _ := newTestBreaker(3, 10*time.Second)

	for i := 0; i < 2; i++ {
		breaker.RecordFailure("10.0.0.1:8080")
	}
	open, failures := breaker.State("10.0.0.1:8080")
	if open {
		t.Error("未达到阈值不应熔断")
	}
	if failures != 2 {
		t.Errorf("连续失败计数 = %d，期望 2", failures)
	}
	if !breaker.Allow("10.0.0.1:8080") {
		t.Error("未熔断的实例应可选取")
	}

	breaker.RecordFailure("10.0.0.1:8080")
	open, _ = breaker.State("10.0.0.1:8080")
	if !open {
		t.Fatal("达到阈值应打开熔断")
	}
	if breaker.Allow("10.0.0.1:8080") {
		t.Error("熔断中的实例不应被选取")
	}
}

func TestBreakerSuccessResets(t *testing.T) {
	breaker, _ := newTestBreaker(3, 10*time.Second)

	breaker.RecordFailure("10.0.0.1:8080")
	breaker.RecordFailure("10.0.0.1:8080")
	breaker.RecordSuccess("10.0.0.1:8080")

	_, failures := breaker.State("10.0.0.1:8080")
	if failures != 0 {
		t.Errorf("成功后计数应清零，实际 %d", failures)
	}

	// 清零后再失败两次不会立即熔断。
	breaker.RecordFailure("10.0.0.1:8080")
	breaker.RecordFailure("10.0.0.1:8080")
	if open, _ := breaker.State("10.0.0.1:8080"); open {
		t.Error("计数被清零后不应立即熔断")
	}
}

func TestBreakerHalfOpenAfterCooldown(t *testing.T) {
	breaker, clock := newTestBreaker(1, 10*time.Second)

	breaker.RecordFailure("10.0.0.1:8080")
	if breaker.Allow("10.0.0.1:8080") {
		t.Fatal("熔断后不应放行")
	}

	// 冷却未结束：持续拒绝。
	clock.now = clock.now.Add(9 * time.Second)
	if breaker.Allow("10.0.0.1:8080") {
		t.Error("冷却期内不应放行")
	}

	// 冷却结束：半开放行一个请求试探。
	clock.now = clock.now.Add(2 * time.Second)
	if !breaker.Allow("10.0.0.1:8080") {
		t.Error("冷却结束后应放行试探请求")
	}

	// 试探成功即解除熔断。
	breaker.RecordSuccess("10.0.0.1:8080")
	if open, _ := breaker.State("10.0.0.1:8080"); open {
		t.Error("试探成功后应解除熔断")
	}

	// 试探失败则重新熔断。
	breaker.RecordFailure("10.0.0.1:8080")
	if open, _ := breaker.State("10.0.0.1:8080"); !open {
		t.Error("试探失败后应重新熔断")
	}
}

func TestBreakerAvailableFiltersOpenInstances(t *testing.T) {
	breaker, _ := newTestBreaker(1, 10*time.Second)
	breaker.RecordFailure("10.0.0.1:8080")

	instances := []discovery.Instance{
		{Host: "10.0.0.1", Port: 8080, Healthy: true, Enabled: true},
		{Host: "10.0.0.2", Port: 8080, Healthy: true, Enabled: true},
	}

	available := breaker.Available(instances)
	if len(available) != 1 || available[0].Host != "10.0.0.2" {
		t.Errorf("应只保留未熔断实例，实际 %+v", available)
	}
}

func TestBreakerNilIsSafe(t *testing.T) {
	var breaker *Breaker
	if !breaker.Allow("10.0.0.1:8080") {
		t.Error("nil 熔断器应放行全部实例")
	}
	breaker.RecordFailure("10.0.0.1:8080")
	breaker.RecordSuccess("10.0.0.1:8080")
	breaker.Reset()
	if open, failures := breaker.State("10.0.0.1:8080"); open || failures != 0 {
		t.Error("nil 熔断器状态应为空")
	}
}

func TestBreakerIsolatesPerInstance(t *testing.T) {
	breaker, _ := newTestBreaker(1, 10*time.Second)

	breaker.RecordFailure("10.0.0.1:8080")
	if open, _ := breaker.State("10.0.0.2:8080"); open {
		t.Error("熔断不应影响其它实例")
	}
}
