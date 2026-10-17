// Package health 提供健康检查与就绪检查。
//
// 两条关键约束：
//  1. /healthz 永不探测下游——下游抖动导致入口实例被摘除，会把局部故障放大成整体故障；
//  2. /readyz 默认只反映自身状态，仅在 READYZ_PROBE_DOWNSTREAM=true 时才探测下游，
//     供部署平台按需选择严格或宽松的就绪判定。
package health

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"

	"newx-gateway/internal/contracts"
	"newx-gateway/internal/discovery"
	"newx-gateway/internal/httpx"
	"newx-gateway/internal/transport"
)

// Prober 探测下游是否可达。由转发器实现，从而复用同一套连接池与超时设置。
type Prober interface {
	Probe(ctx context.Context, baseURL string) error
}

// Module 是健康检查模块。
type Module struct {
	Version          string
	Env              string
	ProbeDownstream  bool
	CheckDownstreams []Downstream
	Prober           Prober
	Resolver         discovery.Resolver
}

// Downstream 是被探测的下游目标。
type Downstream struct {
	AppCode string
	BaseURL string
}

// New 依据配置构造健康检查模块。
func New(version, env string, probeDownstream bool, downstreams []Downstream, prober Prober) Module {
	return Module{
		Version:          version,
		Env:              env,
		ProbeDownstream:  probeDownstream,
		CheckDownstreams: downstreams,
		Prober:           prober,
	}
}

// Name 实现 transport.Module。
func (m Module) Name() string { return contracts.FamilyHealth }

// Register 实现 transport.Module。
func (m Module) Register(h *server.Hertz, _ transport.Deps) {
	h.GET(contracts.PrefixHealthz, m.handleHealthz)
	h.GET(contracts.PrefixReadyz, m.handleReadyz)
}

// HealthResponse 是 /healthz 的响应体。
type HealthResponse struct {
	Status    string `json:"status"`
	Version   string `json:"version"`
	Env       string `json:"env"`
	Timestamp string `json:"timestamp"`
}

// ReadyResponse 是 /readyz 的响应体。
type ReadyResponse struct {
	Status    string         `json:"status"`
	Version   string         `json:"version"`
	Env       string         `json:"env"`
	Timestamp string         `json:"timestamp"`
	Checks    []ReadyCheck   `json:"checks,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

// ReadyCheck 是单个下游的探测结果。
type ReadyCheck struct {
	AppCode   string `json:"app_code"`
	BaseURL   string `json:"base_url"`
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
}

func (m Module) handleHealthz(_ context.Context, c *app.RequestContext) {
	httpx.SetRoute(c, contracts.FamilyHealth)
	httpx.WriteJSON(c, http.StatusOK, HealthResponse{
		Status:    "ok",
		Version:   m.Version,
		Env:       m.Env,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

func (m Module) handleReadyz(ctx context.Context, c *app.RequestContext) {
	httpx.SetRoute(c, contracts.FamilyHealth)

	response := ReadyResponse{
		Status:    "ready",
		Version:   m.Version,
		Env:       m.Env,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	if m.ProbeDownstream {
		response.Checks = m.probeAll(ctx)
		for _, check := range response.Checks {
			if !check.Reachable {
				response.Status = "degraded"
				break
			}
		}
	}

	status := http.StatusOK
	if response.Status != "ready" {
		status = http.StatusServiceUnavailable
	}
	httpx.WriteJSON(c, status, response)
}

// probeAll 并发探测已配置应用，任一可用实例连通即认为该应用可达。
func (m Module) probeAll(ctx context.Context) []ReadyCheck {
	checks := make([]ReadyCheck, len(m.CheckDownstreams))
	done := make(chan struct{}, len(m.CheckDownstreams))

	for index, downstream := range m.CheckDownstreams {
		checks[index] = ReadyCheck{AppCode: downstream.AppCode, BaseURL: downstream.BaseURL}
		go func(index int, downstream Downstream) {
			defer func() { done <- struct{}{} }()
			probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			if err := m.probeDownstream(probeCtx, downstream); err != nil {
				checks[index].Error = err.Error()
				return
			}
			checks[index].Reachable = true
		}(index, downstream)
	}

	for range m.CheckDownstreams {
		<-done
	}
	return checks
}

func (m Module) probeDownstream(ctx context.Context, downstream Downstream) error {
	if m.Prober == nil {
		return fmt.Errorf("prober unavailable")
	}
	if m.Resolver == nil {
		if downstream.BaseURL == "" {
			return fmt.Errorf("应用 %q 未配置服务解析器或静态地址", downstream.AppCode)
		}
		return m.Prober.Probe(ctx, downstream.BaseURL)
	}

	instances, err := m.Resolver.Resolve(ctx, downstream.AppCode)
	if err != nil {
		return err
	}
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, len(instances))
	pending := 0
	for _, instance := range instances {
		if !instance.Usable() {
			continue
		}
		pending++
		go func(target string) {
			results <- m.Prober.Probe(probeCtx, target)
		}(instance.URL())
	}
	if pending == 0 {
		return fmt.Errorf("应用 %q 没有可用实例", downstream.AppCode)
	}
	var lastErr error
	for range pending {
		select {
		case err := <-results:
			if err == nil {
				return nil
			}
			lastErr = err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return lastErr
}
