package middleware

import (
	"context"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"

	"newx-gateway/internal/contracts"
	"newx-gateway/internal/httpx"
)

// BodyLimit 按 Content-Length 提前拒绝超大请求体。
//
// 与 Hertz 的 WithMaxRequestBodySize 是两道防线，职责不同：
//   - Hertz 选项在协议层拦截，避免读入超大 body，是兜底；
//   - 本中间件给出统一的 40002 错误码，是面向客户端的语义层。
//
// 上传通道使用更大的上限：设计文档允许 2GB 上传，
// 若用普通通道的 1MB 去卡上传，素材上传会直接不可用。
//
// 分块传输（Content-Length 为 -1）无法提前判断，交由协议层兜底。
func BodyLimit(maxBytes int64, uploadPaths []string, uploadMaxBytes int64) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		limit := LimitFor(string(c.Path()), maxBytes, uploadPaths, uploadMaxBytes)
		if limit > 0 {
			if length := c.Request.Header.ContentLength(); length > 0 && int64(length) > limit {
				httpx.WriteError(c, contracts.CodeBodyTooLarge)
				return
			}
		}
		c.Next(ctx)
	}
}

// LimitFor 返回该请求路径适用的体积上限。导出以便单测直接覆盖各分支。
func LimitFor(path string, maxBytes int64, uploadPaths []string, uploadMaxBytes int64) int64 {
	if uploadMaxBytes > maxBytes && IsUploadPath(path, uploadPaths) {
		return uploadMaxBytes
	}
	return maxBytes
}

// IsUploadPath 判断路径是否命中上传通道规则。比较大小写不敏感。
func IsUploadPath(path string, uploadPaths []string) bool {
	normalized := strings.ToLower(path)
	for _, fragment := range uploadPaths {
		if fragment == "" {
			continue
		}
		if strings.Contains(normalized, strings.ToLower(fragment)) {
			return true
		}
	}
	return false
}

// isUploadPath 判断路径是否命中上传通道规则（内部别名，避免改调用点）。
func isUploadPath(path string, uploadPaths []string) bool {
	return IsUploadPath(path, uploadPaths)
}
