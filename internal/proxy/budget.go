package proxy

import (
	"net/url"
	"strings"
)

// 请求类型。设计文档 §6 的四级预算按请求类型划分，入口不做业务路由登记，
// 因此用「方法 + 路径片段」把请求归到某一档。
const (
	// KindRead 是普通读取（GET/HEAD/OPTIONS）。
	KindRead = "read"
	// KindWrite 是普通写入（POST/PUT/PATCH/DELETE）。
	KindWrite = "write"
	// KindGenerate 是长耗时生成类请求。
	KindGenerate = "generate"
	// KindUpload 是大文件上传类请求。
	KindUpload = "upload"
)

// BudgetRules 是请求分类规则：路径命中片段即归入对应档位。
type BudgetRules struct {
	GeneratePaths []string
	UploadPaths   []string
	// StreamPaths 是流式接口（SSE、MCP 长连接）的路径片段。
	// 命中后不套用整体超时预算，避免 AI 对话、MCP 会话被 60s 读预算截断。
	StreamPaths []string
}

// IsStream 判断请求是否期望流式响应。
//
// 两种判定方式：
//  1. 客户端声明 Accept: text/event-stream（EventSource、MCP 客户端都会带）；
//  2. 路径命中 STREAM_PATHS（覆盖客户端不带 Accept 的旧实现，如 /sse、/mcp）。
//
// 只要命中一种就按流式处理：连接与响应头仍受常规预算约束，
// 响应体读取不再有整体超时（见 Options.StreamBudget）。
func IsStream(path, accept string, rules BudgetRules) bool {
	if strings.Contains(strings.ToLower(accept), "text/event-stream") {
		return true
	}
	if decoded, err := url.PathUnescape(path); err == nil {
		path = decoded
	}
	return matchAny(strings.ToLower(path), rules.StreamPaths)
}

// KindOf 判定请求类型。
//
// 规则（按优先级）：
//  1. 路径命中上传片段 → upload；
//  2. 路径命中生成片段 → generate；
//  3. 其余按方法分：GET/HEAD/OPTIONS 走读预算，写方法走写预算。
//
// 路径使用入口侧原始路径判断，不依赖上游改写结果，保证规则可预测。
func KindOf(method, path string, rules BudgetRules) string {
	if decoded, err := url.PathUnescape(path); err == nil {
		path = decoded
	}
	normalized := strings.ToLower(path)

	if matchAny(normalized, rules.UploadPaths) {
		return KindUpload
	}
	if matchAny(normalized, rules.GeneratePaths) {
		return KindGenerate
	}

	switch strings.ToUpper(method) {
	case "GET", "HEAD", "OPTIONS":
		return KindRead
	default:
		return KindWrite
	}
}

func matchAny(path string, fragments []string) bool {
	for _, fragment := range fragments {
		if fragment == "" {
			continue
		}
		if strings.Contains(path, strings.ToLower(fragment)) {
			return true
		}
	}
	return false
}
