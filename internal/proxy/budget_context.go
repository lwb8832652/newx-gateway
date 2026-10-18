package proxy

import (
	"context"
	"sync"
	"time"
)

// budgetContext 是可解除的请求预算上下文。
//
// 为什么不用 context.WithTimeout：SSE 是否流式，有时在响应头到达后才能确定
// （Content-Type: text/event-stream），而 WithTimeout 的 deadline 一旦创建就无法撤销，
// 超过预算必然切断长连接。本实现把计时器独立出来：
//   - 响应头到达前：到点取消，Err() 返回 DeadlineExceeded，与 WithTimeout 语义一致；
//   - 响应头到达后：确认是流式响应时调用 disarm，计时器停止，只保留父 context
//     （客户端断开、进程退出）的取消能力。
type budgetContext struct {
	parent   context.Context
	done     chan struct{}
	deadline time.Time

	mu      sync.Mutex
	err     error
	stopped bool
	timer   *time.Timer
}

// newBudgetContext 创建预算上下文；budget <= 0 表示没有截止时间。
func newBudgetContext(parent context.Context, budget time.Duration) (*budgetContext, context.CancelFunc) {
	c := &budgetContext{
		parent: parent,
		done:   make(chan struct{}),
	}
	if budget > 0 {
		c.deadline = time.Now().Add(budget)
		c.timer = time.AfterFunc(budget, func() {
			c.cancel(context.DeadlineExceeded)
		})
	}

	// 父 context 取消（客户端断开、服务关闭）必须继续生效，
	// 这是流式响应解除预算后唯一的终止来源。
	go func() {
		select {
		case <-parent.Done():
			err := parent.Err()
			if err == nil {
				err = context.Canceled
			}
			c.cancel(err)
		case <-c.done:
		}
	}()

	return c, func() { c.cancel(context.Canceled) }
}

// Deadline 实现 context.Context。disarm 之后返回「无截止时间」，
// 避免 HTTP 传输层继续按旧 deadline 设置连接超时。
func (c *budgetContext) Deadline() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped || c.deadline.IsZero() {
		return time.Time{}, false
	}
	return c.deadline, true
}

// Done 实现 context.Context。
func (c *budgetContext) Done() <-chan struct{} { return c.done }

// Err 实现 context.Context。
func (c *budgetContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Value 实现 context.Context，直接透传父 context 的值。
func (c *budgetContext) Value(key any) any { return c.parent.Value(key) }

// disarm 停止预算计时器：响应体不再有整体超时。
// 父 context 的取消仍然有效，客户端断开时请求会正常终止。
func (c *budgetContext) disarm() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped = true
	if c.timer != nil {
		c.timer.Stop()
	}
}

// cancel 只生效一次。disarm 之后到达的计时器回调会被忽略，
// 避免「刚确认是流式、计时器恰好到点」时误杀长连接。
func (c *budgetContext) cancel(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	if c.stopped && err == context.DeadlineExceeded {
		return
	}
	c.err = err
	close(c.done)
}
