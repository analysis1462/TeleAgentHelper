package teleagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// tokenRefreshPath 是网页端/桌面端共用的登录态续期接口。
	tokenRefreshPath = "/superCowork/sapi/cowork/auth/token-refresh"
	// refreshAbsoluteLimit 是服务端允许的会话最长寿命（约 90 天）。
	refreshAbsoluteLimit = 90 * 24 * time.Hour
)

// refreshData 是续期接口 data 段的字段。
type refreshData struct {
	AccessToken       string `json:"accessToken"`
	ExpiresIn         int64  `json:"expiresIn"`
	AbsoluteExpiresAt int64  `json:"absoluteExpiresAt"`
}

// ErrRefreshUnavailable 表示续期接口认为该登录会话已不存在（例如 token 已经被轮换过）。
// 这种情况下 token 本身在过期前仍能用于管理接口，只是有效期无法再延长。
var ErrRefreshUnavailable = errors.New("teleagent: 登录态无法续期（服务器已无该会话记录），当前 token 在过期前仍可用")

// RefreshSession 用当前 token 调续期接口，成功时把新 token 装回客户端并返回会话副本。
// 续期被拒时返回 ErrRefreshUnavailable；需要重新登录时返回 ErrSessionExpired。
func (c *Client) RefreshSession(ctx context.Context) (Session, error) {
	if strings.TrimSpace(c.session.Token) == "" {
		return Session{}, ErrSessionExpired
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.origin+tokenRefreshPath, http.NoBody)
	if err != nil {
		return Session{}, fmt.Errorf("teleagent: 构造续期请求: %w", err)
	}
	c.applyHeaders(request)
	// 桌面端续期用的是秒级时间戳与真实操作系统类型，这里按同样形态覆盖。
	request.Header.Set("X-Timestamp", strconv.FormatInt(c.now().Unix(), 10))
	request.Header.Set("X-OS-Type", runtimeOSName())
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return Session{}, fmt.Errorf("teleagent: 请求续期: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if readErr != nil {
		return Session{}, fmt.Errorf("teleagent: 读取续期响应: %w", readErr)
	}
	env, parseErr := parseEnvelope(raw)
	if parseErr != nil {
		if response.StatusCode == http.StatusUnauthorized {
			return Session{}, ErrSessionExpired
		}
		return Session{}, &APIError{Status: response.StatusCode, Message: strings.TrimSpace(string(raw)), Path: tokenRefreshPath}
	}
	if response.StatusCode == http.StatusUnauthorized {
		// HTTP 401 的响应体常带 40115（服务器上已无该会话记录），此时 token 本身仍然可用。
		if refreshRotatedCode(rawCode(env.Code)) {
			return Session{}, ErrRefreshUnavailable
		}
		return Session{}, ErrSessionExpired
	}
	if err := checkEnvelope(response.StatusCode, tokenRefreshPath, env); err != nil {
		if apiErr, ok := err.(*APIError); ok {
			if refreshRotatedCode(apiErr.Code) {
				return Session{}, ErrRefreshUnavailable
			}
			if refreshReauthCode(apiErr.Code) {
				return Session{}, ErrSessionExpired
			}
		}
		return Session{}, err
	}
	var data refreshData
	if len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, &data); err != nil {
			return Session{}, fmt.Errorf("teleagent: 解析续期响应: %w", err)
		}
	}
	session := c.session
	session.CapturedAt = c.now()
	if token := strings.TrimSpace(data.AccessToken); token != "" && token != rawToken(session.Token) {
		session.Token = NewToken(token, session.Tenant)
	} else {
		session.Token = strings.TrimSpace(session.Token)
	}
	expiresAt := c.now().Add(refreshExpiry(data.ExpiresIn))
	if absolute := time.Unix(data.AbsoluteExpiresAt, 0); data.AbsoluteExpiresAt > 0 && absolute.Before(expiresAt) {
		expiresAt = absolute
	}
	session.ExpiresAt = expiresAt
	if meta, metaErr := ParseTokenMeta(session.Token); metaErr == nil && !meta.ExpiresAt.IsZero() {
		if meta.ExpiresAt.Before(session.ExpiresAt) {
			session.ExpiresAt = meta.ExpiresAt
		}
	}
	session.Normalize()
	c.SetSession(session)
	return c.session, nil
}

// refreshRotatedCode 判断续期返回的业务码是否表示“该会话已被轮换、不可再续期”。
func refreshRotatedCode(code string) bool { return code == "40115" }

// refreshReauthCode 判断续期返回的业务码是否表示“必须重新登录”。
func refreshReauthCode(code string) bool {
	switch code {
	case "40112", "40113", "40114", "40115", "40116":
		return true
	}
	return false
}

// refreshExpiry 把服务端给的 expiresIn 换算成有效期；异常值退回 7 天。
func refreshExpiry(seconds int64) time.Duration {
	if seconds <= 0 {
		return 7 * 24 * time.Hour
	}
	span := time.Duration(seconds) * time.Second
	if span > refreshAbsoluteLimit {
		return refreshAbsoluteLimit
	}
	return span
}

// rawToken 去掉 "<tenant>:" 前缀，便于与服务端返回的裸 JWT 比较。
func rawToken(token string) string {
	trimmed := strings.TrimSpace(token)
	if index := strings.LastIndex(trimmed, ":"); index >= 0 {
		return trimmed[index+1:]
	}
	return trimmed
}

// runtimeOSName 把 Go 的 GOOS 映射成服务端认的操作系统标识。
func runtimeOSName() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "darwin":
		return "darwin"
	default:
		return "linux"
	}
}
