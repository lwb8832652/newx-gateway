// Package openapi 提供外部系统接入家族的入口路由壳。
//
// 路由形态：/open-api/{app_code}/v1/*
// 认证策略（接入后）：API Client + 请求签名 + 防重放 + 独立配额。
//
// 默认关闭（OPENAPI_ENABLED=false）：对外开放接口属于风险面最大的通道，
// 必须在鉴权、签名与配额全部就绪后才允许开启。
// 关闭时不注册路由，请求由兜底处理器返回「路由不存在」。
package openapi

import (
	"newx-gateway/internal/contracts"
	"newx-gateway/internal/transport"
)

// New 构造 OpenAPI 家族模块。enabled 来自 OPENAPI_ENABLED。
func New(enabled bool) transport.FamilyModule {
	return transport.FamilyModule{
		Family:    contracts.FamilyOpenAPI,
		Prefix:    contracts.PrefixOpenAPI,
		Versioned: true,
		Enabled:   enabled,
	}
}
