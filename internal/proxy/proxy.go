// Package proxy 提供最小可用的下游 HTTP 转发。
//
// 边界：
//   - 只做协议搬运：请求方法、路径、Query、请求头、请求体原样送上游；
//   - 响应同样原样回写（状态码、响应头、流式响应体），不在入口缓冲或改写业务响应；
//   - 只负责错误分类：拨号失败 → 50201，超时 → 50401，其它 → 50202。
//
// 刻意不引入 Hertz 的 HTTP 客户端：转发需要精确控制连接池、超时与流式拷贝，
// 标准库 net/http 语义更直观，也便于用 httptest 做端到端测试。
package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cloudwego/hertz/pkg/app"

	"newx-gateway/internal/contracts"
)

// hopByHopHeaders 是逐跳头（RFC 7230 §6.1），转发时必须剔除，
// 否则会把上游的连接语义错误地传递给客户端。
var hopByHopHeaders = map[string]bool{
	"connection":          true,
	"proxy-connection":    true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

// Options 是转发器的运行参数。
type Options struct {
	// DialTimeout 是建立连接的超时。拨号失败必须快速返回 50201，
	// 不能占用路由的整体预算——否则下游宕机会把入口的线程/连接耗尽。
	DialTimeout time.Duration
	// Budgets 是按路由家族的整体超时预算；未命中家族使用 DefaultBudget。
	Budgets map[string]time.Duration
	// DefaultBudget 是默认整体超时。
	DefaultBudget time.Duration
	// Rules 决定请求被归入哪一档预算（读/写/生成/上传）。
	Rules BudgetRules
	// MaxIdleConns 与 MaxIdleConnsPerHost 控制连接池，避免每个下游都新建连接。
	MaxIdleConns        int
	MaxIdleConnsPerHost int
	// TrustProxyHeaders 决定是否写入 X-Forwarded-* 头。
	// 只有入口前方确有可信代理时才开启，否则客户端可伪造来源 IP。
	TrustProxyHeaders bool
}

// Target 是上游目标。
type Target struct {
	Scheme   string
	Host     string
	Path     string
	RawQuery string
}

// URL 拼出完整上游地址。
func (t Target) URL() string {
	scheme := t.Scheme
	if scheme == "" {
		scheme = "http"
	}
	path := t.Path
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	url := scheme + "://" + t.Host + path
	if t.RawQuery != "" {
		url += "?" + t.RawQuery
	}
	return url
}

// Proxy 是转发器。
type Proxy struct {
	client        *http.Client
	noRetryClient *http.Client
	opts          Options
}

// New 构建转发器。零值字段一律回落到默认值，避免调用方漏配导致行为不确定。
func New(opts Options) *Proxy {
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = 2 * time.Second
	}
	if opts.MaxIdleConns <= 0 {
		opts.MaxIdleConns = 256
	}
	if opts.MaxIdleConnsPerHost <= 0 {
		opts.MaxIdleConnsPerHost = 64
	}
	if opts.DefaultBudget <= 0 {
		opts.DefaultBudget = 60 * time.Second
	}

	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: opts.DialTimeout}).DialContext,
		MaxIdleConns:        opts.MaxIdleConns,
		MaxIdleConnsPerHost: opts.MaxIdleConnsPerHost,
		IdleConnTimeout:     90 * time.Second,
		// 响应头超时交由调用方通过 context 控制，这里不重复设置。
		ResponseHeaderTimeout: 0,
		DisableCompression:    true,
	}

	noRetryTransport := transport.Clone()
	noRetryTransport.DisableKeepAlives = true
	noRetryTransport.Protocols = new(http.Protocols)
	noRetryTransport.Protocols.SetHTTP1(true)
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	return &Proxy{
		client: &http.Client{
			Transport:     transport,
			CheckRedirect: noRedirect,
			// 不设 Timeout：整体预算由 Forward 中的 context 控制，
			// 否则长耗时生成请求会被客户端级超时误杀。
			Timeout: 0,
		},
		noRetryClient: &http.Client{Transport: noRetryTransport, CheckRedirect: noRedirect},
		opts:          opts,
	}
}

// Error 是转发失败的结构化错误，携带应返回给客户端的错误码。
type Error struct {
	Code contracts.Code
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }

func (e *Error) Unwrap() error { return e.Err }

// Forward 把当前请求转发到上游，并把响应原样写回。
//
// 约定：返回的 error 一定是 *Error，调用方据此取出错误码；
// 响应体由 Hertz 在返回后消费，流中错误由 Hertz 处理，不能再返回或改写状态码。
func (p *Proxy) Forward(ctx context.Context, rc *app.RequestContext, target Target, family string) error {
	method := string(rc.Method())
	path := target.Path
	budget := p.budget(family, method, path)
	requestCtx, cancel := context.WithTimeout(ctx, budget)
	transferred := false
	defer func() {
		if !transferred {
			cancel()
		}
	}()

	requestBody, contentLength, err := requestBodyOf(rc)
	if err != nil {
		return &Error{Code: contracts.CodeDownstreamBad, Err: err}
	}

	request, err := http.NewRequestWithContext(requestCtx, method, target.URL(), requestBody)
	if err != nil {
		return &Error{Code: contracts.CodeDownstreamBad, Err: err}
	}
	// 流式请求体不会自带 Content-Length，必须显式声明，否则会退化成 chunked，
	// 部分下游不接受分块上传。
	request.ContentLength = contentLength

	copyRequestHeaders(request, rc, p.opts.TrustProxyHeaders)

	client := p.client
	if !p.CanRetry(method, string(rc.Path())) {
		client = p.noRetryClient
	}
	response, err := client.Do(request)
	if err != nil {
		return classify(err)
	}

	// 注意：不能在这里 defer response.Body.Close()。
	// Hertz 是在处理器返回之后才读取 bodyStream 并写出的，提前关闭会让读取失败
	// （http: read on closed response body），表现为连接被重置、客户端收到 EOF。
	// 响应体和超时取消权一并移交给 Hertz；预算持续覆盖响应体读取。
	response.Body = &cancelBody{ReadCloser: response.Body, cancel: cancel}
	writeResponse(rc, response)
	transferred = true
	return nil
}

// CanRetry 仅允许普通读取重试，生成和上传路径优先于 HTTP 方法。
func (p *Proxy) CanRetry(method, path string) bool {
	return KindOf(method, path, p.opts.Rules) == KindRead
}

// cancelBody 不增加缓冲或后台 goroutine；读完、读失败或 Hertz 关闭流时释放预算。
// Hertz 在客户端写出失败时也会关闭流，Close 先取消以中止仍在进行的上游读取。
// context.CancelFunc 可重复调用，EOF 后的 Close 仍须关闭底层响应体。
type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.cancel()
	}
	return n, err
}

func (b *cancelBody) Close() error {
	b.cancel()
	return b.ReadCloser.Close()
}

// Probe 探测目标地址是否可达，供就绪检查复用同一个连接池。
// 只关心「能否完成一次 HTTP 交互」，不把 4xx/5xx 判为不可达。
func (p *Proxy) Probe(ctx context.Context, baseURL string) error {
	if p == nil || baseURL == "" {
		return fmt.Errorf("探测目标为空")
	}

	requestCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, baseURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")

	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	// 探测不关心响应内容，直接丢弃并关闭。
	defer response.Body.Close()
	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return err
}

// probeTimeout 是单个探测的超时预算：就绪检查必须快速返回，
// 否则部署平台会先超时，再得到一个无意义的错误。
const probeTimeout = 2 * time.Second

// budget 返回该请求的整体超时。
//
// 取值顺序（从具体到宽泛）：
//  1. 家族级覆盖：MCP 等通道有独立严格预算，不该被请求类型放宽；
//  2. 请求类型档位：read/write/generate/upload；
//  3. 默认值。
func (p *Proxy) budget(family, method, path string) time.Duration {
	if budget, ok := p.opts.Budgets[budgetFamilyKey(family)]; ok && budget > 0 {
		return budget
	}
	if budget, ok := p.opts.Budgets[KindOf(method, path, p.opts.Rules)]; ok && budget > 0 {
		return budget
	}
	if p.opts.DefaultBudget > 0 {
		return p.opts.DefaultBudget
	}
	return 60 * time.Second
}

// budgetFamilyKey 是家族级预算的键，加前缀以和请求类型档位区分开。
func budgetFamilyKey(family string) string { return "family:" + family }

// copyRequestHeaders 复制请求头，剔除逐跳头与 Host。
// Host 必须由 net/http 根据上游地址重新生成，透传会造成虚拟主机错乱。
func copyRequestHeaders(request *http.Request, rc *app.RequestContext, trustProxyHeaders bool) {
	rc.Request.Header.VisitAll(func(key, value []byte) {
		name := strings.ToLower(string(key))
		if hopByHopHeaders[name] || name == "host" || name == "content-length" {
			return
		}
		request.Header.Add(string(key), string(value))
	})

	if trustProxyHeaders {
		if clientIP := rc.ClientIP(); clientIP != "" {
			// 多级代理时必须追加而不是覆盖，否则会丢掉上游代理已经记录的链路。
			request.Header.Set("X-Forwarded-For", appendForwardedFor(request.Header.Get("X-Forwarded-For"), clientIP))
			request.Header.Set("X-Forwarded-Proto", "http")
			request.Header.Set("X-Forwarded-Host", string(rc.Host()))
		}
	}
}

// appendForwardedFor 按 RFC 7239 语义把新地址追加到已有链之后。
func appendForwardedFor(existing, clientIP string) string {
	if existing == "" {
		return clientIP
	}
	return existing + ", " + clientIP
}

// writeResponse 原样回写上游响应。
//
// 响应体使用流式写出（SetBodyStream），不做整体缓冲：
// 大文件、视频分段（Range 206）与长连接流式响应都不会被入口拖慢或占用内存。
func writeResponse(rc *app.RequestContext, response *http.Response) {
	rc.Response.Header.SetStatusCode(response.StatusCode)

	for key, values := range response.Header {
		name := strings.ToLower(key)
		if hopByHopHeaders[name] || name == "content-length" {
			continue
		}
		for _, value := range values {
			rc.Response.Header.Add(key, value)
		}
	}

	// Content-Length 由 Hertz 根据流大小自行处理；仅在已知长度时显式声明，
	// 保证客户端进度条与 Range 请求行为一致。
	size := -1
	if response.ContentLength >= 0 {
		size = int(response.ContentLength)
		rc.Response.Header.Set("Content-Length", strconv.Itoa(size))
	}
	rc.SetBodyStream(response.Body, size)
}

func readerOrNil(reader *bytes.Reader) *bytes.Reader {
	if reader == nil {
		return nil
	}
	return reader
}

// requestBodyOf 取出要转发给上游的请求体。
//
// 优先使用流式请求体：大文件上传若先整体读进内存，单请求就能吃掉 GB 级堆内存。
// 只有非流式（已被 Hertz 缓冲）时才回退到内存副本。
//
// 返回值第二个字段是内容长度；-1 表示长度未知，交给上游按 chunked 处理。
func requestBodyOf(rc *app.RequestContext) (io.Reader, int64, error) {
	declared := int64(rc.Request.Header.ContentLength())

	if rc.Request.IsBodyStream() {
		stream := rc.Request.BodyStream()
		if stream == nil {
			return nil, 0, fmt.Errorf("请求体流为空")
		}
		return stream, declared, nil
	}

	body := rc.Request.Body()
	if len(body) == 0 {
		// 注意：必须返回接口类型的 nil。直接返回 *bytes.Reader 的类型化空指针，
		// 会让 http.NewRequestWithContext 误判为「有请求体」并发出 Content-Length: 0 的请求体。
		return nil, 0, nil
	}
	return bytes.NewReader(body), int64(len(body)), nil
}

// classify 把传输层错误映射为入口错误码。
func classify(err error) *Error {
	// 整体预算耗尽。
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: contracts.CodeDownstreamTimeout, Err: err}
	}

	// 传输层超时（含 DNS 超时）。
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &Error{Code: contracts.CodeDownstreamTimeout, Err: err}
	}

	// 「根本连不上」的各类形态：连接被拒、DNS 失败、无路由。
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return &Error{Code: contracts.CodeDownstreamRefused, Err: err}
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return &Error{Code: contracts.CodeDownstreamRefused, Err: err}
	}

	// 连接建立后被对端关闭、或写入中途被重置：同样属于「连不上」，
	// 值得按幂等重试一次，因此也归为下游不可用而不是笼统的响应异常。
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return &Error{Code: contracts.CodeDownstreamRefused, Err: err}
	}
	var resetErr *net.OpError
	if errors.As(err, &resetErr) && isConnectionReset(resetErr) {
		return &Error{Code: contracts.CodeDownstreamRefused, Err: err}
	}
	if isConnectionReset(err) {
		return &Error{Code: contracts.CodeDownstreamRefused, Err: err}
	}

	return &Error{Code: contracts.CodeDownstreamBad, Err: err}
}

// isConnectionReset 判断错误链中是否包含连接重置。
func isConnectionReset(err error) bool {
	for ; err != nil; err = errors.Unwrap(err) {
		if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) {
			return true
		}
		if opErr, ok := err.(*net.OpError); ok {
			if errors.Is(opErr.Err, syscall.ECONNRESET) || errors.Is(opErr.Err, syscall.EPIPE) {
				return true
			}
		}
	}
	return false
}
