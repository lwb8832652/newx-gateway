// Package discovery 负责把服务名解析为可用实例列表。
//
// 设计约束：
//   - 入口只读注册中心，不注册自己——入口是无状态实例，流量分发由部署平台负责；
//   - 静态地址与 Nacos 发现可以按服务共存，切换只改配置；
//   - 解析失败时优先返回过期缓存，其次才报错：一次注册中心抖动不应让入口整体失败。
package discovery

import (
	"net"
	"strconv"
	"strings"
)

// Instance 是一个下游实例。
type Instance struct {
	// Service 是实例所属的服务名，与注册表中的 app_code 对应。
	Service string
	Host    string
	Port    int
	Scheme  string
	// Weight 是负载均衡权重，<=0 视为无权重（由 LB 退化为均摊）。
	Weight float64
	// Healthy 与 Enabled 来自注册中心；静态地址默认为 true。
	Healthy bool
	Enabled bool
	// Metadata 承载灰度标签：version 用于版本灰度，tag 用于环境/机器定向。
	Metadata map[string]string
}

// MetadataVersion 与 MetadataTag 是灰度使用的 metadata 键，与 yudao 网关保持一致。
const (
	MetadataVersion = "version"
	MetadataTag     = "tag"
)

// Address 返回 host:port。
func (i Instance) Address() string {
	return net.JoinHostPort(i.Host, strconv.Itoa(i.Port))
}

// URL 返回带协议的基址。
func (i Instance) URL() string {
	scheme := i.Scheme
	if scheme == "" {
		scheme = "http"
	}
	return scheme + "://" + i.Address()
}

// Version 返回实例声明的版本，空表示未参与版本灰度。
func (i Instance) Version() string { return i.metadata(MetadataVersion) }

// Tag 返回实例声明的标签，空表示未打标。
func (i Instance) Tag() string { return i.metadata(MetadataTag) }

func (i Instance) metadata(key string) string {
	if i.Metadata == nil {
		return ""
	}
	return strings.TrimSpace(i.Metadata[key])
}

// Usable 报告实例是否可参与负载均衡。
func (i Instance) Usable() bool { return i.Healthy && i.Enabled && i.Port > 0 && i.Host != "" }

// Clone 深拷贝 metadata，避免调用方修改污染缓存。
func (i Instance) Clone() Instance {
	copied := i
	if i.Metadata != nil {
		copied.Metadata = make(map[string]string, len(i.Metadata))
		for key, value := range i.Metadata {
			copied.Metadata[key] = value
		}
	}
	return copied
}
