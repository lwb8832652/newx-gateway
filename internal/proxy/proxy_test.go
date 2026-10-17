package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"

	"newx-gateway/internal/contracts"
)

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
