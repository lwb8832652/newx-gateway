package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"

	"newx-gateway/internal/contracts"
)

func TestForwardDoesNotFollowRedirects(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			t.Run(http.StatusText(status)+"/"+method, func(t *testing.T) {
				var redirected atomic.Int32
				destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					redirected.Add(1)
					_, _ = io.WriteString(w, "redirected")
				}))
				defer destination.Close()
				location := destination.URL + "/target?token=redirect-secret"
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Location", location)
					w.WriteHeader(status)
					_, _ = io.WriteString(w, "redirect response")
				}))
				defer upstream.Close()

				p := New(Options{DefaultBudget: time.Second})
				c := app.NewContext(0)
				c.Request.SetRequestURI("/web/huobao/v1/start")
				c.Request.Header.SetMethod(method)
				c.Request.Header.Set("Authorization", "Bearer private-token")
				c.Request.Header.Set("login-user", "private-identity")
				c.Request.SetBodyString("private-body")
				if err := p.Forward(context.Background(), c, Target{
					Scheme: "http", Host: strings.TrimPrefix(upstream.URL, "http://"), Path: "/start",
				}, "web"); err != nil {
					t.Fatal(err)
				}
				if got := c.Response.StatusCode(); got != status {
					t.Fatalf("重定向状态码 = %d，期望 %d", got, status)
				}
				if got := c.Response.Header.Get("Location"); got != location {
					t.Errorf("Location = %q，期望 %q", got, location)
				}
				body, err := io.ReadAll(c.Response.BodyStream())
				_ = c.Response.CloseBodyStream()
				if err != nil || string(body) != "redirect response" {
					t.Errorf("重定向正文 = %q，err = %v", body, err)
				}
				if got := redirected.Load(); got != 0 {
					t.Errorf("网关跟随了重定向 %d 次", got)
				}
			})
		}
	}
}

func TestForwardDoesNotReplayUnsafeRequests(t *testing.T) {
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/tasks/create"},
		{http.MethodHead, "/agent/run"},
		{http.MethodOptions, "/merge/start"},
		{http.MethodPost, "/dramas"},
		{http.MethodPut, "/dramas/1"},
		{http.MethodGet, "/upload"},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			var attempts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/warm" {
					_, _ = io.WriteString(w, "warm")
					return
				}
				attempts.Add(1)
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
			}))
			defer upstream.Close()
			p := New(Options{DefaultBudget: time.Second, Rules: BudgetRules{
				GeneratePaths: []string{"/tasks", "/agent", "/merge"}, UploadPaths: []string{"/upload"},
			}})
			target := Target{Scheme: "http", Host: strings.TrimPrefix(upstream.URL, "http://"), Path: "/warm"}
			warm := app.NewContext(0)
			warm.Request.Header.SetMethod(http.MethodGet)
			warm.Request.SetRequestURI("/warm")
			if err := p.Forward(context.Background(), warm, target, "web"); err != nil {
				t.Fatalf("预热失败: %v", err)
			}
			if _, err := io.Copy(io.Discard, warm.Response.BodyStream()); err != nil {
				t.Fatal(err)
			}
			_ = warm.Response.CloseBodyStream()

			c := app.NewContext(0)
			c.Request.Header.SetMethod(tc.method)
			c.Request.SetRequestURI(tc.path)
			c.Request.Header.Set("Idempotency-Key", "test-key")
			target.Path = tc.path
			if err := p.Forward(context.Background(), c, target, "web"); err == nil {
				t.Fatal("下游关闭连接时应返回代理错误")
			}
			if got := attempts.Load(); got != 1 {
				t.Errorf("生成/写入请求被底层重放，实际请求 %d 次", got)
			}
		})
	}
}

func TestForwardDelayedStreamAfterReturn(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(20 * time.Millisecond)
		_, _ = io.WriteString(w, "data: second\n\n")
	}))
	defer upstream.Close()
	p := New(Options{DefaultBudget: time.Second})
	defer p.client.CloseIdleConnections()
	rc := newProxyContext()
	defer rc.Response.CloseBodyStream()
	if err := p.Forward(context.Background(), rc, Target{Host: strings.TrimPrefix(upstream.URL, "http://"), Path: "/events"}, "web"); err != nil {
		t.Fatal(err)
	}
	// The upstream sends no body until Forward has returned, as with Hertz's deferred writer.
	close(release)
	body, err := io.ReadAll(rc.Response.BodyStream())
	if err != nil {
		t.Fatalf("read stream after Forward: %v", err)
	}
	if string(body) != "data: first\n\ndata: second\n\n" {
		t.Fatalf("truncated stream: %q", body)
	}
}

// TestForwardStreamSkipsOverallBudget 验证流式响应（SSE / MCP）不被整体预算截断：
// 路径命中 STREAM_PATHS 或请求声明 Accept: text/event-stream 时，
// 响应头到达后停止计时，只受客户端断开约束。
func TestForwardStreamSkipsOverallBudget(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		accept string
	}{
		{name: "path-rule", path: "/sse"},
		{name: "accept-header", path: "/admin-api/ai/chat", accept: "text/event-stream"},
		{name: "mcp-path", path: "/mcp/yudao-ai", accept: "application/json, text/event-stream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "data: first\n\n")
				w.(http.Flusher).Flush()
				// 超过 DefaultBudget 后再发第二条：若预算仍覆盖响应体，这里会读失败。
				time.Sleep(250 * time.Millisecond)
				_, _ = io.WriteString(w, "data: second\n\n")
			}))
			defer upstream.Close()

			p := New(Options{
				DefaultBudget: 80 * time.Millisecond,
				StreamBudget:  0,
				Rules:         BudgetRules{StreamPaths: []string{"/sse", "/mcp"}},
			})
			defer p.client.CloseIdleConnections()

			rc := newProxyContext()
			rc.Request.SetRequestURI(tc.path)
			if tc.accept != "" {
				rc.Request.Header.Set("Accept", tc.accept)
			}
			target := Target{Host: strings.TrimPrefix(upstream.URL, "http://"), Path: tc.path}
			if err := p.Forward(context.Background(), rc, target, "web"); err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(rc.Response.BodyStream())
			if err != nil {
				t.Fatalf("读取流失败: %v", err)
			}
			if !strings.Contains(string(body), "data: first") || !strings.Contains(string(body), "data: second") {
				t.Fatalf("流被截断: %q", body)
			}
			if got := rc.Response.Header.Get("X-Accel-Buffering"); got != "no" {
				t.Errorf("X-Accel-Buffering = %q，期望 no", got)
			}
		})
	}
}

// TestForwardResponseDetectedSSEStopsBudget 覆盖「请求侧没识别出流式，
// 但响应头是 text/event-stream」的场景：响应头到达后必须解除预算，
// 否则 /ai/chat/message/send-stream 这类路径仍会被 60s 截断。
func TestForwardResponseDetectedSSEStopsBudget(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(250 * time.Millisecond)
		_, _ = io.WriteString(w, "data: second\n\n")
	}))
	defer upstream.Close()

	// 路径不含 stream/sse/mcp 关键字，请求也不带 Accept，只能靠响应头识别。
	p := New(Options{DefaultBudget: 80 * time.Millisecond})
	defer p.client.CloseIdleConnections()

	rc := newProxyContext()
	rc.Request.SetRequestURI("/chatdata")
	if err := p.Forward(context.Background(), rc, Target{
		Host: strings.TrimPrefix(upstream.URL, "http://"),
		Path: "/chatdata",
	}, "web"); err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(rc.Response.BodyStream())
	if err != nil {
		t.Fatalf("读取流失败: %v", err)
	}
	if !strings.Contains(string(body), "data: first") || !strings.Contains(string(body), "data: second") {
		t.Fatalf("响应侧 SSE 被预算截断: %q", body)
	}
}

// TestForwardWebSocketHandshakeTimeout 验证上游只建 TCP 连接、不回 101 时，
// 握手阶段会按 TIMEOUT_WS_HANDSHAKE 返回 50401，而不是永久悬挂。
func TestForwardWebSocketHandshakeTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动监听失败: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		// 故意不读请求、不返回 101，模拟「TCP 通但应用层不响应」。
		time.Sleep(time.Second)
		_ = conn.Close()
	}()

	p := New(Options{DialTimeout: 200 * time.Millisecond, HandshakeTimeout: 100 * time.Millisecond})
	rc := newProxyContext()
	rc.Request.SetRequestURI("/infra/ws")
	rc.Request.Header.Set("Upgrade", "websocket")
	rc.Request.Header.Set("Connection", "Upgrade")
	rc.Request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	rc.Request.Header.Set("Sec-WebSocket-Version", "13")

	start := time.Now()
	err = p.ForwardWebSocket(context.Background(), rc, Target{
		Host: listener.Addr().String(),
		Path: "/infra/ws",
	})
	var proxyErr *Error
	if !errors.As(err, &proxyErr) || proxyErr.Code != contracts.CodeDownstreamTimeout {
		t.Fatalf("错误 = %v，期望握手超时 50401", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("握手超时耗时 %s，未按 100ms 生效", elapsed)
	}
}

// TestForwardStreamHeaderPhaseStillBudgeted 验证「无限流」只在响应头到达后成立：
// 上游迟迟不返回响应头时仍按常规预算快速失败，不会永久占住连接。
func TestForwardStreamHeaderPhaseStillBudgeted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := New(Options{
		DefaultBudget: 80 * time.Millisecond,
		StreamBudget:  0,
		Rules:         BudgetRules{StreamPaths: []string{"/sse"}},
	})
	defer p.client.CloseIdleConnections()

	rc := newProxyContext()
	rc.Request.SetRequestURI("/sse")
	err := p.Forward(context.Background(), rc, Target{
		Host: strings.TrimPrefix(upstream.URL, "http://"),
		Path: "/sse",
	}, "web")

	var proxyErr *Error
	if !errors.As(err, &proxyErr) || proxyErr.Code != contracts.CodeDownstreamTimeout {
		t.Fatalf("错误 = %v，期望 50401", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type observedBody struct {
	io.Reader
	closed atomic.Int32
}

func (b *observedBody) Close() error { b.closed.Add(1); return nil }

func newProxyContext() *app.RequestContext {
	rc := app.NewContext(0)
	rc.Request.Header.SetMethod(http.MethodGet)
	return rc
}

func TestForwardStreamContextLifetime(t *testing.T) {
	for _, terminal := range []string{"eof", "read-error", "close", "parent-cancel", "budget"} {
		t.Run(terminal, func(t *testing.T) {
			parent, stop := context.WithCancel(context.Background())
			defer stop()
			p := New(Options{DefaultBudget: 100 * time.Millisecond})
			var requestCtx context.Context
			body := &observedBody{Reader: strings.NewReader("ok")}
			if terminal == "read-error" {
				body.Reader = failingReader{}
			}
			p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				requestCtx = r.Context()
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, ContentLength: -1}, nil
			})
			rc := newProxyContext()
			defer rc.Response.CloseBodyStream()
			if err := p.Forward(parent, rc, Target{Host: "upstream.test"}, "web"); err != nil {
				t.Fatal(err)
			}
			if err := requestCtx.Err(); err != nil {
				t.Fatalf("context canceled before streaming: %v", err)
			}
			switch terminal {
			case "eof", "read-error":
				_, err := io.ReadAll(rc.Response.BodyStream())
				if (terminal == "read-error") != errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("unexpected stream error: %v", err)
				}
			case "close":
				if err := rc.Response.CloseBodyStream(); err != nil {
					t.Fatal(err)
				}
				if body.closed.Load() != 1 {
					t.Fatalf("body closed %d times", body.closed.Load())
				}
			case "parent-cancel":
				stop()
			case "budget":
			}
			select {
			case <-requestCtx.Done():
			case <-time.After(time.Second):
				t.Fatal("request context not released")
			}
			if terminal == "budget" && !errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
				t.Fatalf("budget lost: %v", requestCtx.Err())
			}
		})
	}
}

func TestProbeBodyReadErrors(t *testing.T) {
	for _, mode := range []string{"truncated", "timeout", "status-500"} {
		t.Run(mode, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "truncated" {
					w.Header().Set("Content-Length", "20")
				}
				if mode == "status-500" {
					w.WriteHeader(http.StatusInternalServerError)
				}
				_, _ = io.WriteString(w, "short")
				w.(http.Flusher).Flush()
				if mode == "timeout" {
					<-r.Context().Done()
				}
			}))
			defer upstream.Close()
			p := New(Options{})
			defer p.client.CloseIdleConnections()
			err := p.Probe(context.Background(), upstream.URL)
			switch mode {
			case "truncated":
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("Probe error = %v, want unexpected EOF", err)
				}
			case "timeout":
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("Probe error = %v, want deadline exceeded", err)
				}
			case "status-500":
				if err != nil {
					t.Fatalf("HTTP 500 must remain reachable: %v", err)
				}
			}
		})
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestForwardErrorReleasesContext(t *testing.T) {
	p := New(Options{DefaultBudget: time.Hour})
	var requestCtx context.Context
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requestCtx = r.Context()
		return nil, errors.New("upstream failure")
	})
	err := p.Forward(context.Background(), newProxyContext(), Target{Host: "upstream.test"}, "web")
	var proxyErr *Error
	if !errors.As(err, &proxyErr) || proxyErr.Code != contracts.CodeDownstreamBad {
		t.Fatalf("unexpected error: %v", err)
	}
	if !errors.Is(requestCtx.Err(), context.Canceled) {
		t.Fatalf("failed request context not canceled: %v", requestCtx.Err())
	}
}

func TestForwardStreamBudgetAndCancellation(t *testing.T) {
	for _, cancelParent := range []bool{false, true} {
		t.Run(map[bool]string{false: "budget", true: "parent-cancel"}[cancelParent], func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "first\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer upstream.Close()
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			p := New(Options{DefaultBudget: 200 * time.Millisecond})
			defer p.client.CloseIdleConnections()
			rc := newProxyContext()
			defer rc.Response.CloseBodyStream()
			if err := p.Forward(parent, rc, Target{Host: strings.TrimPrefix(upstream.URL, "http://")}, "web"); err != nil {
				t.Fatal(err)
			}
			first := make([]byte, len("first\n"))
			if _, err := io.ReadFull(rc.Response.BodyStream(), first); err != nil {
				t.Fatal(err)
			}
			if cancelParent {
				cancel()
			}
			_, err := io.ReadAll(rc.Response.BodyStream())
			want := context.DeadlineExceeded
			if cancelParent {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatalf("stream error = %v, want %v", err, want)
			}
			if rc.Response.StatusCode() != http.StatusOK {
				t.Fatalf("streaming failure rewrote upstream status: %d", rc.Response.StatusCode())
			}
		})
	}
}
