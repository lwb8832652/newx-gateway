package discovery

import (
	"context"
	"fmt"
	"net"
	"net/url"
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

// NewStaticResolver 接受 host:port 或完整 HTTP/HTTPS URL，未指定端口时使用协议默认值。
func NewStaticResolver(addresses map[string][]string) (*StaticResolver, error) {
	resolved := make(map[string][]Instance, len(addresses))
	for service, list := range addresses {
		instances := make([]Instance, 0, len(list))
		for _, address := range list {
			endpoint := strings.TrimSpace(address)
			scheme := "http"
			if strings.Contains(endpoint, "://") {
				parsed, err := url.Parse(endpoint)
				if err != nil {
					return nil, fmt.Errorf("服务 %q 的地址 %q 非法: %w", service, address, err)
				}
				scheme = parsed.Scheme
				if scheme != "http" && scheme != "https" {
					return nil, fmt.Errorf("服务 %q 的地址 %q 仅支持 HTTP/HTTPS", service, address)
				}
				port := parsed.Port()
				if port == "" {
					port = "80"
					if scheme == "https" {
						port = "443"
					}
				}
				endpoint = net.JoinHostPort(parsed.Hostname(), port)
			}
			host, port, err := net.SplitHostPort(endpoint)
			if err != nil || host == "" {
				return nil, fmt.Errorf("服务 %q 的地址 %q 需包含主机与有效端口", service, address)
			}
			portNumber, err := strconv.Atoi(port)
			if err != nil || portNumber < 1 || portNumber > 65535 {
				return nil, fmt.Errorf("服务 %q 的地址 %q 端口非法", service, address)
			}
			instances = append(instances, Instance{
				Service: service,
				Host:    host,
				Port:    portNumber,
				Scheme:  scheme,
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
//
// 返回的切片是解析器内部缓存的直接引用（零拷贝）：实例在整条链路上只读，
// 每个请求都深拷贝一遍会让热路径白白产生大量 map 分配。
// 调用方不得修改返回值或其 Metadata。
func (s *StaticResolver) Resolve(_ context.Context, service string) ([]Instance, error) {
	if s == nil {
		return nil, fmt.Errorf("静态解析器未初始化")
	}
	instances, ok := s.addresses[service]
	if !ok {
		return nil, fmt.Errorf("服务 %q 未配置静态地址", service)
	}
	return instances, nil
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

// Multi 按应用编码选择解析器，并映射到实际注册中心服务名。
type Multi struct {
	services map[string]serviceResolver
}

type serviceResolver struct {
	resolver Resolver
	service  string
}

// NewMulti 构建组合解析器。
func NewMulti() *Multi { return &Multi{services: make(map[string]serviceResolver)} }

// Add 为某个服务指定同名解析器。
func (m *Multi) Add(service string, resolver Resolver) {
	m.AddAlias(service, service, resolver)
}

// AddAlias 将应用编码映射到解析器使用的实际服务名。
func (m *Multi) AddAlias(appCode, service string, resolver Resolver) {
	if m == nil || appCode == "" || service == "" || resolver == nil {
		return
	}
	m.services[appCode] = serviceResolver{resolver: resolver, service: service}
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
	return resolver.resolver.Resolve(ctx, resolver.service)
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
