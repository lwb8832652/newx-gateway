package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// checkTokenPath 是 yudao 的令牌校验接口（OAuth2TokenCommonApi.URL_CHECK）。
const checkTokenPath = "/system-api/oauth2/check-token"

// commonResult 是 yudao 统一响应包装：{code, data, message}。
type commonResult struct {
	Code    int             `json:"code"`
	Data    json.RawMessage `json:"data"`
	Message string          `json:"message"`
}

// IsError 报告是否业务错误（code 非 0）。
func (r commonResult) IsError() bool { return r.Code != 0 }

// tokenInfo 是 check-token 返回的令牌信息（OAuth2AccessTokenCheckRespDTO）。
type tokenInfo struct {
	UserID      int64             `json:"userId"`
	UserType    int               `json:"userType"`
	UserInfo    map[string]string `json:"userInfo"`
	TenantID    int64             `json:"tenantId"`
	Scopes      []string          `json:"scopes"`
	ExpiresTime string            `json:"expiresTime"`
}

// StatusUnauthorized 是 yudao 表示「令牌已过期/无效」的业务码。
const statusUnauthorized = 401

// TokenClient 调用 yudao 的 check-token 接口。
type TokenClient struct {
	// Dial 返回鉴权方基址。支持静态地址与注册中心两种来源，
	// 由装配层注入，从而复用入口已有的服务发现与熔断能力。
	Dial       func(ctx context.Context) (string, error)
	Path       string
	Timeout    time.Duration
	HTTPClient *http.Client
}

// NewTokenClient 构建令牌客户端。
func NewTokenClient(dial func(ctx context.Context) (string, error), path string, timeout time.Duration) *TokenClient {
	if path == "" {
		path = checkTokenPath
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &TokenClient{
		Dial:       dial,
		Path:       path,
		Timeout:    timeout,
		HTTPClient: &http.Client{Timeout: timeout},
	}
}

// Check 校验访问令牌并返回登录用户。
//
// 与 yudao 的语义保持一致：令牌无效/过期不返回错误，而是返回 nil 用户，
// 由调用方按「无身份放行」处理——网关不替下游做登录拦截。
func (c *TokenClient) Check(ctx context.Context, tenantID int64, accessToken string) (*LoginUser, error) {
	if c == nil || c.Dial == nil {
		return nil, fmt.Errorf("令牌客户端未初始化")
	}

	base, err := c.Dial(ctx)
	if err != nil {
		return nil, err
	}

	endpoint := strings.TrimSuffix(base, "/") + c.Path +
		"?accessToken=" + url.QueryEscape(accessToken)

	requestCtx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	// tenant-id 头：yudao 用它做多租户隔离，必须随校验请求一起送出。
	if tenantID > 0 {
		request.Header.Set("tenant-id", strconv.FormatInt(tenantID, 10))
	}

	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("check-token 返回 %d: %s", response.StatusCode, truncate(string(payload), 200))
	}

	var result commonResult
	if err := json.Unmarshal(payload, &result); err != nil {
		return nil, fmt.Errorf("解析 check-token 响应失败: %w", err)
	}
	if result.IsError() {
		// 令牌过期/无效：按无身份处理，不算错误。
		if result.Code == statusUnauthorized {
			return nil, nil
		}
		return nil, fmt.Errorf("check-token 业务错误 code=%d message=%s", result.Code, result.Message)
	}
	if len(result.Data) == 0 || string(result.Data) == "null" {
		return nil, nil
	}

	var info tokenInfo
	if err := json.Unmarshal(result.Data, &info); err != nil {
		return nil, fmt.Errorf("解析令牌信息失败: %w", err)
	}
	return toLoginUser(&info)
}

// toLoginUser 把下游 DTO 转成注入用的 LoginUser。
func toLoginUser(info *tokenInfo) (*LoginUser, error) {
	expiresAt, err := parseExpiresTime(info.ExpiresTime)
	if err != nil {
		return nil, err
	}
	return &LoginUser{
		ID:          info.UserID,
		UserType:    info.UserType,
		Info:        info.UserInfo,
		TenantID:    info.TenantID,
		Scopes:      info.Scopes,
		ExpiresTime: expiresAt,
	}, nil
}

// parseExpiresTime 兼容多种时间输出，避免下游改个序列化格式就让身份失效。
func parseExpiresTime(raw string) (time.Time, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return time.Time{}, nil
	}
	layouts := []string{
		"2006-01-02T15:04:05",
		"2006-01-02T15:04:05.999999999",
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if parsed, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("无法解析过期时间 %q", raw)
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "..."
}
