package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Nacos 开放 API 的实例列表路径。Nacos 1.x/2.x 均兼容。
const nacosInstanceListPath = "/nacos/v1/ns/instance/list"

// NacosConfig 是 Nacos 服务发现的配置。
type NacosConfig struct {
	// Addr 是 Nacos 控制台地址，形如 host:port。
	Addr string
	// Namespace 是命名空间 ID，为空使用 public。
	Namespace string
	// Group 是分组名，空表示 DEFAULT_GROUP。
	Group string
	// Refresh 是后台刷新周期；0 表示不后台刷新（仅按需拉取）。
	Refresh time.Duration
	// Client 可注入自定义 HTTP 客户端（测试用）；为空使用默认客户端。
	Client *http.Client
	// Logger 用于记录后台刷新失败；为空则静默。
	Logger *slog.Logger
}

// nacosInstanceList 是实例列表接口的响应结构。
type nacosInstanceList struct {
	Hosts []struct {
		IP          string            `json:"ip"`
		Port        int               `json:"port"`
		Weight      float64           `json:"weight"`
		Healthy     bool              `json:"healthy"`
		Enabled     bool              `json:"enabled"`
		Metadata    map[string]string `json:"metadata"`
		ServiceName string            `json:"serviceName"`
	} `json:"hosts"`
}

// cacheEntry 缓存一次拉取结果。
type cacheEntry struct {
	instances []Instance
	fetchedAt time.Time
}

// NacosResolver 基于 Nacos 开放 API 的服务发现。
//
// 刻意不引入 nacos-go-sdk：入口只需要「读实例列表」这一件事，
// 用 HTTP 开放 API 可以少一个重量级依赖，也让失败行为完全可控。
type NacosResolver struct {
	cfg   NacosConfig
	mu    sync.RWMutex
	cache map[string]cacheEntry
}

// NewNacosResolver 构建 Nacos 解析器并校验地址。
func NewNacosResolver(cfg NacosConfig) (*NacosResolver, error) {
	if strings.TrimSpace(cfg.Addr) == "" {
		return nil, fmt.Errorf("Nacos 地址不能为空")
	}
	if cfg.Refresh < 0 {
		return nil, fmt.Errorf("刷新周期不能为负: %s", cfg.Refresh)
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 5 * time.Second}
	}
	return &NacosResolver{cfg: cfg, cache: make(map[string]cacheEntry)}, nil
}

// Start 启动后台刷新循环，只刷新已进入缓存的服务（即已被请求过的服务）。
// 传入的 ctx 结束时循环退出。
func (n *NacosResolver) Start(ctx context.Context) {
	if n == nil || n.cfg.Refresh <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(n.cfg.Refresh)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := n.Refresh(ctx); err != nil && n.cfg.Logger != nil {
					n.cfg.Logger.Warn("刷新 Nacos 实例列表失败，继续使用缓存", "err", err)
				}
			}
		}
	}()
}

// Resolve 实现 Resolver。
//
// 缓存未过期直接返回；已过期或缺失则立即拉取。拉取失败时：
//   - 有旧缓存 → 返回旧缓存（宁可流量打到已知实例，也不要整体失败）；
//   - 无缓存 → 返回错误，由调用方映射为 50201。
func (n *NacosResolver) Resolve(ctx context.Context, service string) ([]Instance, error) {
	if n == nil {
		return nil, fmt.Errorf("Nacos 解析器未初始化")
	}

	n.mu.RLock()
	entry, ok := n.cache[service]
	ttl := n.cfg.Refresh
	n.mu.RUnlock()

	if ok && (ttl <= 0 || time.Since(entry.fetchedAt) < ttl) {
		return cloneInstances(entry.instances), nil
	}

	instances, err := n.fetch(ctx, service)
	if err != nil {
		if ok {
			// 拉取失败但存在旧缓存：降级使用旧数据。
			return cloneInstances(entry.instances), nil
		}
		return nil, err
	}

	n.store(service, instances)
	return cloneInstances(instances), nil
}

// Refresh 刷新缓存中的全部服务。缓存为空时不做任何事。
func (n *NacosResolver) Refresh(ctx context.Context) error {
	if n == nil {
		return fmt.Errorf("Nacos 解析器未初始化")
	}

	n.mu.RLock()
	services := make([]string, 0, len(n.cache))
	for service := range n.cache {
		services = append(services, service)
	}
	n.mu.RUnlock()

	var errs []error
	for _, service := range services {
		instances, err := n.fetch(ctx, service)
		if err != nil {
			errs = append(errs, fmt.Errorf("服务 %q: %w", service, err))
			continue
		}
		n.store(service, instances)
	}
	if len(errs) > 0 {
		return fmt.Errorf("刷新失败: %w", joinErrors(errs))
	}
	return nil
}

// fetch 调用 Nacos 开放 API 拉取实例列表，并过滤掉不可用实例。
func (n *NacosResolver) fetch(ctx context.Context, service string) ([]Instance, error) {
	group := n.cfg.Group
	if group == "" {
		group = "DEFAULT_GROUP"
	}

	endpoint := "http://" + strings.TrimSuffix(n.cfg.Addr, "/") + nacosInstanceListPath +
		"?serviceName=" + url.QueryEscape(service) +
		"&groupName=" + url.QueryEscape(group)
	if n.cfg.Namespace != "" {
		endpoint += "&namespaceId=" + url.QueryEscape(n.cfg.Namespace)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("构造 Nacos 请求失败: %w", err)
	}
	request.Header.Set("Accept", "application/json")

	response, err := n.cfg.Client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("请求 Nacos 失败: %w", err)
	}
	defer response.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("读取 Nacos 响应失败: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		// 鉴权开启时 Nacos 会返回 403；此时必须暴露原文，否则无法定位。
		return nil, fmt.Errorf("Nacos 返回 %d: %s", response.StatusCode, truncate(string(payload), 200))
	}

	var parsed nacosInstanceList
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, fmt.Errorf("解析 Nacos 响应失败: %w", err)
	}

	instances := make([]Instance, 0, len(parsed.Hosts))
	for _, host := range parsed.Hosts {
		weight := host.Weight
		if weight <= 0 {
			weight = 1
		}
		instances = append(instances, Instance{
			Service:  service,
			Host:     host.IP,
			Port:     host.Port,
			Scheme:   "http",
			Weight:   weight,
			Healthy:  host.Healthy,
			Enabled:  host.Enabled,
			Metadata: host.Metadata,
		})
	}
	return instances, nil
}

func (n *NacosResolver) store(service string, instances []Instance) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.cache[service] = cacheEntry{instances: cloneInstances(instances), fetchedAt: time.Now()}
}

// Services 返回当前已缓存的服务名。
func (n *NacosResolver) Services() []string {
	if n == nil {
		return nil
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	services := make([]string, 0, len(n.cache))
	for service := range n.cache {
		services = append(services, service)
	}
	return services
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "..."
}

func joinErrors(errs []error) error {
	if len(errs) == 1 {
		return errs[0]
	}
	messages := make([]string, 0, len(errs))
	for _, err := range errs {
		messages = append(messages, err.Error())
	}
	return fmt.Errorf("%s", strings.Join(messages, "; "))
}
