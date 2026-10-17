// Package health 提供健康检查与就绪检查。
//
// 两条关键约束：
//  1. /healthz 永不探测下游——下游抖动导致入口实例被摘除，会把局部故障放大成整体故障；
//  2. /readyz 默认只反映自身状态，仅在 READYZ_PROBE_DOWNSTREAM=true 时才探测下游，
//     供部署平台按需选择严格或宽松的就绪判定。
package health

import (
	"context"
	"net/http"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"

	"newx-gateway/internal/contracts"
	"newx-gateway/internal/httpx"
	"newx-gateway/internal/transport"
)

// probeTimeout 是单个下游探测的超时预算。就绪检查必须快速返回，
// 否则部署平台会先超时，再得到一个无意义的错误。
const probeTimeout = 2 * time.Second

// Module 是健康检查模块。
type Module struct {
	Version          string
	Env              string
	ProbeDownstream  bool
	CheckDownstreams []Downstream
}

// Downstream 是被探测的下游目标。
type Downstream struct {
	AppCode string
	BaseURL string
}

// New 依据配置构造健康检查模块。
func New(version, env string, probeDownstream bool, downstreams []Downstream) Module {
	return Module{
		Version:          version,
		Env:              env,
		ProbeDownstream:  probeDownstream,
		CheckDownstreams: downstreams,
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

// probeAll 并发探测全部已配置地址的下游。
func (m Module) probeAll(ctx context.Context) []ReadyCheck {
	checks := make([]ReadyCheck, len(m.CheckDownstreams))
	done := make(chan struct{}, len(m.CheckDownstreams))

	for index, downstream := range m.CheckDownstreams {
		checks[index] = ReadyCheck{AppCode: downstream.AppCode, BaseURL: downstream.BaseURL}
		if downstream.BaseURL == "" {
			// 未配置地址表示下游尚未接入，视为可达，避免脚手架阶段恒 degraded。
			checks[index].Reachable = true
			done <- struct{}{}
			continue
		}

		go func(index int, target string) {
			defer func() { done <- struct{}{} }()
			reachable, err := probe(ctx, target)
			checks[index].Reachable = reachable
			if err != nil {
				checks[index].Error = err.Error()
			}
		}(index, downstream.BaseURL)
	}

	for range m.CheckDownstreams {
		<-done
	}
	return checks
}

// probe 探测目标是否可达。只关心「能否建立并完成一次 HTTP 交互」，
// 不把下游的 4xx/5xx 判定为不可达——那是下游的业务状态，不是连通性。
func probe(ctx context.Context, baseURL string) (bool, error) {
	requestCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, baseURL, nil)
	if err != nil {
		return false, err
	}

	client := &http.Client{Timeout: probeTimeout}
	response, err := client.Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	return true, nil
}
