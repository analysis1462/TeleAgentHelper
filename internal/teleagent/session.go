// Package teleagent 对接天翼 TeleAgent（agent.teleai.com.cn）的网页会话：
// 它复用浏览器登录得到的 cookie/x-token，负责云电脑保活与每日积分签到。
package teleagent

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

const sessionVersion = 1

// Cookie 是浏览器会话里的一条 cookie，用于回放登录态。
type Cookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain,omitempty"`
	Path     string `json:"path,omitempty"`
	Expires  int64  `json:"expires,omitempty"`
	Secure   bool   `json:"secure,omitempty"`
	HTTPOnly bool   `json:"http_only,omitempty"`
}

// TokenMeta 是 x-token 内 JWT 载荷中关心的字段。
type TokenMeta struct {
	Tenant      string
	AccountID   string
	AccountName string
	UserName    string
	IssuedAt    time.Time
	ExpiresAt   time.Time
}

// Session 是持久化的登录态。Token 形如 "<tenant>:<jwt>"。
type Session struct {
	Version     int       `json:"version"`
	Token       string    `json:"token"`
	Tenant      string    `json:"tenant,omitempty"`
	DeviceID    string    `json:"device_id,omitempty"`
	AppVersion  string    `json:"app_version,omitempty"`
	AccountID   string    `json:"account_id,omitempty"`
	AccountName string    `json:"account_name,omitempty"`
	Cookies     []Cookie  `json:"cookies,omitempty"`
	CapturedAt  time.Time `json:"captured_at"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
}

// Empty 判断会话中是否完全没有凭据。
func (s Session) Empty() bool {
	return strings.TrimSpace(s.Token) == "" && len(s.Cookies) == 0
}

// Valid 判断会话是否可用于请求（不判断服务端是否已吊销）。
func (s Session) Valid(now time.Time) bool {
	if s.Empty() || strings.TrimSpace(s.Token) == "" {
		return false
	}
	if s.ExpiresAt.IsZero() {
		return true
	}
	return now.Before(s.ExpiresAt)
}

// CookieHeader 拼出 Cookie 请求头。
func (s Session) CookieHeader() string {
	if len(s.Cookies) == 0 {
		return ""
	}
	parts := make([]string, 0, len(s.Cookies))
	for _, cookie := range s.Cookies {
		if strings.TrimSpace(cookie.Name) == "" {
			continue
		}
		parts = append(parts, cookie.Name+"="+cookie.Value)
	}
	return strings.Join(parts, "; ")
}

// Cookie 按名字查找 cookie。
func (s Session) Cookie(name string) (string, bool) {
	for _, cookie := range s.Cookies {
		if cookie.Name == name {
			return cookie.Value, true
		}
	}
	return "", false
}

// Normalize 补齐缺省字段并从 token 推导租户与过期时间。
func (s *Session) Normalize() {
	if s.Version == 0 {
		s.Version = sessionVersion
	}
	s.Token = strings.TrimSpace(s.Token)
	if s.AppVersion == "" {
		s.AppVersion = DefaultAppVersion
	}
	if s.DeviceID == "" {
		s.DeviceID = randomUUID()
	}
	if s.Token == "" {
		return
	}
	meta, err := ParseTokenMeta(s.Token)
	if err != nil {
		return
	}
	if s.Tenant == "" {
		s.Tenant = meta.Tenant
	}
	if s.AccountID == "" {
		s.AccountID = meta.AccountID
	}
	if s.AccountName == "" {
		s.AccountName = meta.AccountName
	}
	if s.ExpiresAt.IsZero() {
		s.ExpiresAt = meta.ExpiresAt
	}
}

// NewToken 把裸 JWT 组装成服务端要求的 "<tenant>:<jwt>" 形式。
func NewToken(raw, tenant string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Count(raw, ".") == 2 && strings.Contains(raw, ":") {
		// 已经是 tenant:jwt
		return raw
	}
	if tenant == "" {
		if meta, err := ParseTokenMeta(raw); err == nil {
			tenant = meta.Tenant
		}
	}
	if tenant == "" {
		return raw
	}
	return tenant + ":" + raw
}

// ParseTokenMeta 解出 x-token 中 JWT 的载荷字段。
func ParseTokenMeta(token string) (TokenMeta, error) {
	trimmed := strings.TrimSpace(token)
	if index := strings.LastIndex(trimmed, ":"); index >= 0 {
		trimmed = trimmed[index+1:]
	}
	segments := strings.Split(trimmed, ".")
	if len(segments) != 3 {
		return TokenMeta{}, errInvalidToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(segments[1], "="))
	if err != nil {
		return TokenMeta{}, errInvalidToken
	}
	var claims struct {
		Tenant      string `json:"tenant"`
		AccountID   string `json:"accountId"`
		AccountName string `json:"accountName"`
		UserName    string `json:"userName"`
		Expiry      int64  `json:"exp"`
		IssuedAt    int64  `json:"iat"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return TokenMeta{}, errInvalidToken
	}
	meta := TokenMeta{
		Tenant:      claims.Tenant,
		AccountID:   claims.AccountID,
		AccountName: claims.AccountName,
		UserName:    claims.UserName,
	}
	if claims.Expiry > 0 {
		meta.ExpiresAt = time.Unix(claims.Expiry, 0)
	}
	if claims.IssuedAt > 0 {
		meta.IssuedAt = time.Unix(claims.IssuedAt, 0)
	}
	return meta, nil
}
