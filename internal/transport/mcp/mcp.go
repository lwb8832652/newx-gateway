// Package mcp 提供 MCP 家族的入口路由壳。
//
// 路由形态：/mcp/{app_code}/*
// 认证策略（接入后）：独立凭证 + 工具白名单 + 严格超时。
// 本期硬性约束：不允许触发付费生成工具。
//
// 关闭时（MCP_ENABLED=false）不注册任何路由，请求由兜底处理器返回「路由不存在」——
// 关闭的通道不应暴露自身存在。
package mcp

import (
	"newx-gateway/internal/contracts"
	"newx-gateway/internal/transport"
)

// New 构造 MCP 家族模块。enabled 来自 MCP_ENABLED。
func New(enabled bool) transport.FamilyModule {
	return transport.FamilyModule{
		Family:    contracts.FamilyMCP,
		Prefix:    contracts.PrefixMCP,
		Versioned: false,
		Enabled:   enabled,
	}
}
