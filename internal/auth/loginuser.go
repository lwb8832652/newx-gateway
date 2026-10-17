// Package auth 对齐 yudao-cloud 的身份传递契约，授权由下游决定。
//
// yudao-gateway 的 TokenAuthenticationFilter 做法（源码核实）：
//   - 读取 Authorization: Bearer <token>，无 token 直接放行，不拦截；
//   - 有 token 则调用下游 check-token 接口换取 LoginUser，本地缓存 1 分钟；
//   - 把 LoginUser 序列化为 JSON 并 URL-encode 后写入 login-user 请求头；
//     tenant-id 单独一个头；
//   - 校验失败或过期同样放行，由下游服务自行判断接口是否需要登录。
//
// 本包沿用 login-user 头的字段名与编码方式，不声称逐项复刻 Java 实现——
// 它是芋道框架的私有约定（定义在 yudao-spring-boot-starter-security），
// 不是 OAuth2/OIDC 标准，字段或编码差一点，下游就解析不出身份。
package auth

import (
	"encoding/json"
	"net/url"
	"time"
)

// LoginUser 是注入到 login-user 头中的登录用户。
//
// 字段名、类型与顺序都必须与 yudao 的 LoginUser 保持一致：
// id/userType/info/tenantId/scopes/expiresTime。
type LoginUser struct {
	ID          int64             `json:"id"`
	UserType    int               `json:"userType"`
	Info        map[string]string `json:"info"`
	TenantID    int64             `json:"tenantId"`
	Scopes      []string          `json:"scopes"`
	ExpiresTime time.Time         `json:"expiresTime"`
}

// Empty 报告是否是「无身份」的空用户（token 缺失或校验失败时使用）。
func (u *LoginUser) Empty() bool { return u == nil || u.ID == 0 }

// Expired 报告用户是否已过期。
func (u *LoginUser) Expired(now time.Time) bool {
	if u == nil || u.ExpiresTime.IsZero() {
		return true
	}
	return !now.Before(u.ExpiresTime)
}

// HeaderValue 返回写入 login-user 头的值：JSON 序列化后再 URL-encode。
//
// expiresTime 使用 JSON 数字毫秒时间戳，与 yudao 注册的
// TimestampLocalDateTimeSerializer/Deserializer 一致，而非 Jackson 默认 ISO 格式。
func (u *LoginUser) HeaderValue() (string, error) {
	normalized := u.normalized()
	payload, err := json.Marshal(struct {
		ID          int64             `json:"id"`
		UserType    int               `json:"userType"`
		Info        map[string]string `json:"info"`
		TenantID    int64             `json:"tenantId"`
		Scopes      []string          `json:"scopes"`
		ExpiresTime int64             `json:"expiresTime"`
	}{normalized.ID, normalized.UserType, normalized.Info, normalized.TenantID, normalized.Scopes, normalized.ExpiresTime.UnixMilli()})
	if err != nil {
		return "", err
	}
	return url.QueryEscape(string(payload)), nil
}

// normalized 返回用于序列化的副本，把零值字段收敛成 yudao 的形状，
// 避免出现 {"id":0,"userType":0,"info":null,...} 这类下游要判空的字段。
func (u *LoginUser) normalized() LoginUser {
	copied := *u
	if copied.Info == nil {
		copied.Info = map[string]string{}
	}
	if copied.Scopes == nil {
		copied.Scopes = []string{}
	}
	return copied
}

// ParseLoginUser 从 login-user 头值还原用户对象，主要用于测试与自检。
func ParseLoginUser(headerValue string) (*LoginUser, error) {
	decoded, err := url.QueryUnescape(headerValue)
	if err != nil {
		return nil, err
	}
	var user LoginUser
	var wire struct {
		*LoginUser
		ExpiresTime json.RawMessage `json:"expiresTime"`
	}
	wire.LoginUser = &user
	if err := json.Unmarshal([]byte(decoded), &wire); err != nil {
		return nil, err
	}
	user.ExpiresTime, err = parseJSONExpiresTime(wire.ExpiresTime)
	if err != nil {
		return nil, err
	}
	return &user, nil
}

// FormatExpiresTime 仅用于人类可读的本地时间显示，不是 yudao 的传输格式。
func FormatExpiresTime(value time.Time) string {
	return value.Format("2006-01-02T15:04:05")
}
