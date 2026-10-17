package discovery

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
)

// Resolver 把服务名解析为实例列表。
type Resolver interface {
	Resolve(ctx context.Context, service string) ([]Instance, error)
}

// StaticResolver 从静态配置解析实例，用于尚未引入注册中心的阶段。
type StaticResolver struct {
	addresses map[string][]Instance
}

// NewStaticResolver 用「服务名 → host:port 列表」构建静态解析器。
// 地址格式非法会直接返回错误，让配置问题在启动期暴露。
func NewStaticResolver(addresses map[string][]string) (*StaticResolver, error) {
	resolved := make(map[string][]Instance, len(addresses))
	for service, list := range addresses {
		instances := make([]Instance, 0, len(list))
		for _, address := range list {
			host, port, err := net.SplitHostPort(strings.TrimSpace(address))
			if err != nil {
				return nil, fmt.Errorf("服务 %q 的地址 %q 需为 host:port 形式: %w", service, address, err)
			}
			portNumber, err := strconv.Atoi(port)
			if err != nil || portNumber < 1 || portNumber > 65535 {
				return nil, fmt.Errorf("服务 %q 的地址 %q 端口非法", service, address)
			}
			instances = append(instances, Instance{
				Service: service,
				Host:    host,
				Port:    portNumber,
				Scheme:  "http",
				Weight:  1,
				Healthy: true,
				Enabled: true,
			})
		}
		resolved[service] = instances
	}
	return &StaticResolver{addresses: resolved}, nil
}

// Resolve 实现 Resolver。
func (s *StaticResolver) Resolve(_ context.Context, service string) ([]Instance, error) {
	if s == nil {
		return nil, fmt.Errorf("静态解析器未初始化")
	}
	instances, ok := s.addresses[service]
	if !ok {
		return nil, fmt.Errorf("服务 %q 未配置静态地址", service)
	}
	return cloneInstances(instances), nil
}

// Services 返回已配置的服务名，按字典序排列。
func (s *StaticResolver) Services() []string {
	if s == nil {
		return nil
	}
	services := make([]string, 0, len(s.addresses))
	for service := range s.addresses {
		services = append(services, service)
	}
	sort.Strings(services)
	return services
}

// Multi 按服务选择解析器，让静态地址与注册中心发现共存。
type Multi struct {
	services map[string]Resolver
}

// NewMulti 构建组合解析器。
func NewMulti() *Multi { return &Multi{services: make(map[string]Resolver)} }

// Add 为某个服务指定解析器。
func (m *Multi) Add(service string, resolver Resolver) {
	if m == nil || service == "" || resolver == nil {
		return
	}
	m.services[service] = resolver
}

// Resolve 实现 Resolver。
func (m *Multi) Resolve(ctx context.Context, service string) ([]Instance, error) {
	if m == nil {
		return nil, fmt.Errorf("解析器未初始化")
	}
	resolver, ok := m.services[service]
	if !ok {
		return nil, fmt.Errorf("服务 %q 未配置任何解析来源", service)
	}
	return resolver.Resolve(ctx, service)
}

// Services 返回已配置解析来源的服务名，按字典序排列。
func (m *Multi) Services() []string {
	if m == nil {
		return nil
	}
	services := make([]string, 0, len(m.services))
	for service := range m.services {
		services = append(services, service)
	}
	sort.Strings(services)
	return services
}

func cloneInstances(instances []Instance) []Instance {
	copied := make([]Instance, 0, len(instances))
	for _, instance := range instances {
		copied = append(copied, instance.Clone())
	}
	return copied
}
