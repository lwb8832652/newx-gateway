// Package registry 维护 app_code 到下游的映射。
//
// 入口只认注册表里存在的应用，未注册的应用一律拒绝——这样既避免未知前缀被
// 透传到任意后端，也让「新增一个应用」变成一条配置而不是一处代码改动。
//
// 一条注册记录由三部分构成：
//   - Code：应用编码，同时是 URL 前缀与下游服务名；
//   - BaseURL / Service：静态地址或注册中心服务名，二者至少有一个（由 config 校验）；
//   - UpstreamPath：上游路径前缀，用于把入口路径改写成下游路径，
//     例如入口 /admin/yudao-system/v1/user/page → 上游 /admin-api/system/user/page。
package registry

import (
	"errors"
	"fmt"
	"strings"

	"newx-gateway/internal/config"
)

// Spec 是一条应用注册记录。
type Spec struct {
	Code    string
	BaseURL string
	// Service 是注册中心里的服务名；为空表示使用静态地址。
	Service string
	// UpstreamPath 是上游路径前缀，为空表示原样透传。
	UpstreamPath string
}

// App 是注册表中的只读应用信息。
type App struct {
	Code         string
	BaseURL      string
	Service      string
	UpstreamPath string
}

// Routable 报告该应用是否具备可用的下游来源。
func (a App) Routable() bool { return a.BaseURL != "" || a.Service != "" }

// StaticAddress 返回静态地址（host:port），仅静态来源有效。
func (a App) StaticAddress() string { return hostPortOf(a.BaseURL) }

// Registry 是应用注册表。构建后不可变，可并发读。
type Registry struct {
	apps  map[string]App
	order []string
}

// New 构建注册表并完成校验：编码格式、重复项、空表。
func New(specs []Spec) (*Registry, error) {
	if len(specs) == 0 {
		return nil, errors.New("应用注册表不能为空")
	}

	result := &Registry{
		apps:  make(map[string]App, len(specs)),
		order: make([]string, 0, len(specs)),
	}

	var errs []error
	for _, spec := range specs {
		code := strings.TrimSpace(spec.Code)
		if !config.IsValidAppCode(code) {
			errs = append(errs, fmt.Errorf("应用编码非法: %q", spec.Code))
			continue
		}
		if _, exists := result.apps[code]; exists {
			errs = append(errs, fmt.Errorf("应用编码重复: %q", code))
			continue
		}
		if err := validateSpec(spec); err != nil {
			errs = append(errs, err)
			continue
		}
		result.apps[code] = App{
			Code:         code,
			BaseURL:      strings.TrimSpace(spec.BaseURL),
			Service:      strings.TrimSpace(spec.Service),
			UpstreamPath: normalizePath(spec.UpstreamPath),
		}
		result.order = append(result.order, code)
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return result, nil
}

func validateSpec(spec Spec) error {
	baseURL := strings.TrimSpace(spec.BaseURL)
	if baseURL != "" && !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return fmt.Errorf("应用 %q 的静态地址需为 http/https: %q", spec.Code, baseURL)
	}
	if path := strings.TrimSpace(spec.UpstreamPath); path != "" && !strings.HasPrefix(path, "/") {
		return fmt.Errorf("应用 %q 的上游路径需以 / 开头: %q", spec.Code, path)
	}
	return nil
}

// normalizePath 去掉末尾斜杠，保证路径拼接时不会出现 //。
func normalizePath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	trimmed = strings.TrimSuffix(trimmed, "/")
	if trimmed == "" {
		return ""
	}
	return trimmed
}

// SpecsFromConfig 把配置中的应用列表转换为注册表条目，避免各处重复转换逻辑。
func SpecsFromConfig(apps []config.AppConfig) []Spec {
	specs := make([]Spec, 0, len(apps))
	for _, app := range apps {
		specs = append(specs, Spec{
			Code:         app.Code,
			BaseURL:      app.BaseURL,
			Service:      app.Service,
			UpstreamPath: app.UpstreamPath,
		})
	}
	return specs
}

// Lookup 按编码查询应用。第二个返回值表示是否命中。
func (r *Registry) Lookup(code string) (App, bool) {
	if r == nil {
		return App{}, false
	}
	app, ok := r.apps[strings.TrimSpace(code)]
	return app, ok
}

// All 按注册顺序返回全部应用。返回切片为副本，调用方修改不影响注册表。
func (r *Registry) All() []App {
	if r == nil {
		return nil
	}
	apps := make([]App, 0, len(r.order))
	for _, code := range r.order {
		apps = append(apps, r.apps[code])
	}
	return apps
}

// Len 返回已注册应用数量。
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	return len(r.apps)
}

// Codes 按注册顺序返回全部应用编码。
func (r *Registry) Codes() []string {
	if r == nil {
		return nil
	}
	codes := make([]string, len(r.order))
	copy(codes, r.order)
	return codes
}

// hostPortOf 提取 URL 的主机部分，用于日志字段 upstream_host。
func hostPortOf(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	slash := strings.Index(rawURL, "://")
	if slash < 0 {
		return ""
	}
	rest := rawURL[slash+3:]
	if at := strings.IndexAny(rest, "/?#"); at >= 0 {
		rest = rest[:at]
	}
	return rest
}
