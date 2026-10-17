// Package web 提供 Web / Electron 家族的入口路由壳。
//
// 路由形态：/web/{app_code}/v1/*
// 认证策略（接入后）：用户 JWT + 应用 Scope + 基础限流。
package web

import (
	"newx-gateway/internal/contracts"
	"newx-gateway/internal/transport"
)

// New 构造 Web 家族模块。Web 家族始终启用，是否已接下游由 handler 实现决定。
func New() transport.FamilyModule {
	return transport.FamilyModule{
		Family:    contracts.FamilyWeb,
		Prefix:    contracts.PrefixWeb,
		Versioned: true,
		Enabled:   true,
	}
}
