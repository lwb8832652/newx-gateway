package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/network"

	"newx-gateway/internal/contracts"
)

// maxWebSocketRejectBody 是握手被拒时最多回写给客户端的响应体。
// 拒绝响应通常只有一段 JSON 文案，限制上限避免异常下游用大包撑爆内存。
const maxWebSocketRejectBody = 1 << 20

// ForwardWebSocket 转发 WebSocket 握手，并在 101 之后劫持连接做双向字节搬运。
//
// 与普通转发的关键差异：
//   - 握手必须保留 Upgrade / Connection / Sec-WebSocket-* 等逐跳头，
//     而普通转发按 RFC 7230 会剔除它们，因此这里单独构造上游请求；
//   - 握手成功后调用 Hertz 的 Hijack，把客户端连接交给本函数管理，
//     期间不再受框架读写超时与连接池约束；
//   - 不参与重试：握手一旦写给某个实例，就不能换实例重放。
func (p *Proxy) ForwardWebSocket(ctx context.Context, rc *app.RequestContext, target Target) error {
	dialer := &net.Dialer{Timeout: p.opts.DialTimeout}
	upstream, err := dialer.DialContext(ctx, "tcp", target.Host)
	if err != nil {
		return &Error{Code: contracts.CodeDownstreamRefused, Err: err}
	}

	// 握手未成功前，上游连接由本函数负责释放；
	// 101 之后所有权移交给 hijack 回调，避免提前关闭正在搬运的连接。
	transferred := false
	defer func() {
		if !transferred {
			_ = upstream.Close()
		}
	}()

	handshake, err := p.buildHandshake(rc, target)
	if err != nil {
		return &Error{Code: contracts.CodeDownstreamBad, Err: err}
	}

	// 握手阶段必须有独立超时：上游可能只建立 TCP 连接，既不返回 101 也不断开。
	// 这里把 deadline 设在连接上，覆盖握手写入与响应读取；101 之后由 hijack 回调清除。
	deadline := time.Now().Add(p.opts.HandshakeTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = upstream.SetDeadline(deadline)

	if err := handshake.Write(upstream); err != nil {
		return &Error{Code: webSocketErrorCode(err), Err: fmt.Errorf("写入 WebSocket 握手失败: %w", err)}
	}

	reader := bufio.NewReader(upstream)
	response, err := http.ReadResponse(reader, handshake)
	if err != nil {
		return &Error{Code: webSocketErrorCode(err), Err: fmt.Errorf("读取 WebSocket 握手响应失败: %w", err)}
	}

	if response.StatusCode != http.StatusSwitchingProtocols {
		// 上游拒绝升级：按普通响应回写，把 4xx/5xx 语义原样交给客户端。
		// 这里必须先把响应体读完再关闭连接，否则客户端会收到被截断的响应。
		body, readErr := io.ReadAll(io.LimitReader(response.Body, maxWebSocketRejectBody))
		_ = response.Body.Close()
		if readErr != nil {
			return &Error{Code: contracts.CodeDownstreamBad, Err: fmt.Errorf("读取 WebSocket 拒绝响应失败: %w", readErr)}
		}
		writeBufferedResponse(rc, response, body)
		return nil
	}

	// 101 响应头必须原样带回：Upgrade / Connection / Sec-WebSocket-Accept
	// 在普通转发里属于逐跳头，但握手阶段它们就是协议本身。
	rc.Response.SetStatusCode(http.StatusSwitchingProtocols)
	for key, values := range response.Header {
		for _, value := range values {
			rc.Response.Header.Add(key, value)
		}
	}
	// 101 没有响应体，残留的 Content-Length 会让部分客户端误判响应结束位置。
	rc.Response.Header.Del("Content-Length")

	transferred = true
	rc.Hijack(func(client network.Conn) {
		defer upstream.Close()
		// Hijack 后连接生命周期由本函数管理，清除框架留下的超时设置。
		_ = client.SetReadTimeout(0)
		_ = client.SetWriteTimeout(0)
		_ = upstream.SetDeadline(time.Time{})

		done := make(chan struct{}, 2)
		go func() {
			_, _ = io.Copy(upstream, client)
			done <- struct{}{}
		}()
		go func() {
			// reader 可能已缓冲 101 响应之后的早期帧，必须从 reader 读，
			// 直接从 upstream 读会丢掉这部分数据。
			_, _ = io.Copy(client, reader)
			done <- struct{}{}
		}()

		// 任一方向结束（客户端关闭或上游关闭）就同时关闭两端，
		// 让另一个 goroutine 立即从阻塞中退出。
		<-done
		_ = client.Close()
		_ = upstream.Close()
	})
	return nil
}

// webSocketErrorCode 把握手超时归类为 50401，其余失败归类为 50202。
func webSocketErrorCode(err error) contracts.Code {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return contracts.CodeDownstreamTimeout
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return contracts.CodeDownstreamTimeout
	}
	return contracts.CodeDownstreamBad
}

// buildHandshake 构造写给上游的握手请求。
//
// 与普通转发不同，这里保留全部请求头（含 Upgrade / Connection /
// Sec-WebSocket-*），只改写 Host——握手协议依赖这些逐跳头。
func (p *Proxy) buildHandshake(rc *app.RequestContext, target Target) (*http.Request, error) {
	parsed, err := url.Parse(target.URL())
	if err != nil {
		return nil, err
	}

	request := &http.Request{
		Method:     string(rc.Method()),
		URL:        parsed,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header, 16),
		Host:       target.Host,
	}
	rc.Request.Header.VisitAll(func(key, value []byte) {
		name := string(key)
		switch strings.ToLower(name) {
		case "host", "content-length":
			// Host 由上游地址重新生成；握手请求没有请求体。
			return
		}
		request.Header.Add(name, string(value))
	})

	// 与普通转发保持一致：可信代理场景下追加来源链。
	if p.opts.TrustProxyHeaders {
		if clientIP := rc.ClientIP(); clientIP != "" {
			request.Header.Set("X-Forwarded-For", appendForwardedFor(request.Header.Get("X-Forwarded-For"), clientIP))
			request.Header.Set("X-Forwarded-Proto", "http")
			request.Header.Set("X-Forwarded-Host", string(rc.Host()))
		}
	}
	return request, nil
}

// writeBufferedResponse 回写已缓冲的非 101 响应（握手被拒）。
// 响应体已整体读出，因此可以安全关闭上游连接。
func writeBufferedResponse(rc *app.RequestContext, response *http.Response, body []byte) {
	rc.Response.SetStatusCode(response.StatusCode)
	for key, values := range response.Header {
		name := strings.ToLower(key)
		if hopByHopHeaders[name] || name == "content-length" {
			continue
		}
		for _, value := range values {
			rc.Response.Header.Add(key, value)
		}
	}
	rc.Response.SetBody(body)
	if len(body) > 0 {
		rc.Response.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	}
}
