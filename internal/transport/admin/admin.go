// Package admin 提供管理后台家族的入口路由壳。
//
// 路由形态：/admin/{app_code}/v1/*
// 认证策略（接入后）：管理员 JWT + RBAC + 强制审计 + 独立限流。
//
// 与 Web 家族刻意分离：管理侧的错误语义、限流阈值与审计要求都不同，
// 后续接入 Admin Service 时只改本包的处理器，不影响 Web 流量。
package admin

import (
	"newx-gateway/internal/contracts"
	"newx-gateway/internal/transport"
)

// New 构造 Admin 家族模块。
func New() transport.FamilyModule {
	return transport.FamilyModule{
		Family:    contracts.FamilyAdmin,
		Prefix:    contracts.PrefixAdmin,
		Versioned: true,
		Enabled:   true,
	}
}
