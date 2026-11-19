package teleagent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultOrigin 是 TeleAgent 网页端源站。
	DefaultOrigin = "https://agent.teleai.com.cn"
	// DefaultAppVersion 与网页端 x-app-version 保持一致。
	DefaultAppVersion = "3.1.0-rc.1"
	// cloudAuthExpiredCode 是管理面返回的"云端登录已失效"业务码。
	cloudAuthExpiredCode = "4001"
	defaultTimeout       = 30 * time.Second
	defaultUserAgent     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36 Edg/141.0.0.0"
	maxResponseBytes     = 8 << 20
)

var (
	errInvalidToken = errors.New("teleagent: token 格式不正确")
	// ErrSessionExpired 表示云端登录态失效，需要重新登录。
	ErrSessionExpired = errors.New("teleagent: 登录状态已失效，请重新登录")
)

// APIError 是服务端返回的失败结果。
type APIError struct {
	Status  int
	Code    string
	Message string
	Path    string
}

func (e *APIError) Error() string {
	message := e.Message
	if message == "" {
		message = http.StatusText(e.Status)
	}
	if e.Code != "" {
		return fmt.Sprintf("teleagent: %s 返回 %d/%s：%s", e.Path, e.Status, e.Code, message)
	}
	return fmt.Sprintf("teleagent: %s 返回 %d：%s", e.Path, e.Status, message)
}

// ClientOptions 是构造 Client 的参数。
type ClientOptions struct {
	Origin     string
	HTTPClient *http.Client
	Session    Session
	Now        func() time.Time
	Nonce      func() string
	UserAgent  string
}

// Client 是带登录态的 TeleAgent HTTP 客户端。
type Client struct {
	origin    string
	http      *http.Client
	session   Session
	now       func() time.Time
	nonce     func() string
	userAgent string
}

// NewClient 按选项构造客户端。
func NewClient(options ClientOptions) *Client {
	origin := strings.TrimRight(strings.TrimSpace(options.Origin), "/")
	if origin == "" {
		origin = DefaultOrigin
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	nonce := options.Nonce
	if nonce == nil {
		nonce = randomUUID
	}
	agent := options.UserAgent
	if agent == "" {
		agent = defaultUserAgent
	}
	session := options.Session
	session.Normalize()
	return &Client{
		origin:    origin,
		http:      httpClient,
		session:   session,
		now:       now,
		nonce:     nonce,
		userAgent: agent,
	}
}

// Origin 返回客户端使用的源站。
func (c *Client) Origin() string { return c.origin }

// Session 返回当前登录态副本。
func (c *Client) Session() Session { return c.session }

// SetSession 替换登录态。
func (c *Client) SetSession(session Session) {
	session.Normalize()
	c.session = session
}

// SessionExpired 判断当前会话是否在本地就已经过期。
func (c *Client) SessionExpired() bool { return !c.session.Valid(c.now()) }

func (c *Client) applyHeaders(request *http.Request) {
	request.Header.Set("Accept", "application/json, text/plain, */*")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", c.origin)
	request.Header.Set("Referer", c.origin+"/teleagent")
	request.Header.Set("User-Agent", c.userAgent)
	request.Header.Set("X-Client-Platform", "web")
	request.Header.Set("X-App-Version", c.session.AppVersion)
	request.Header.Set("X-OS-Type", "web")
	request.Header.Set("X-SuperAgent-Device-Id", c.session.DeviceID)
	request.Header.Set("X-Timestamp", strconv.FormatInt(c.now().Unix(), 10))
	request.Header.Set("X-Nonce", c.nonce())
	request.Header.Set("X-Channel-Id", "")
	request.Header.Set("X-Runtime-Env", "")
	request.Header.Set("X-Token", c.session.Token)
	if cookie := c.session.CookieHeader(); cookie != "" {
		request.Header.Set("Cookie", cookie)
	}
}

type envelopeError struct {
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
}

type envelope struct {
	Code    json.RawMessage `json:"code"`
	Msg     string          `json:"msg"`
	Message string          `json:"message"`
	Error   *envelopeError  `json:"error"`
	Data    json.RawMessage `json:"data"`
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("teleagent: 编码请求体: %w", err)
		}
		payload = encoded
	}
	var reader io.Reader
	if len(payload) > 0 {
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.origin+path, reader)
	if err != nil {
		return fmt.Errorf("teleagent: 构造请求: %w", err)
	}
	c.applyHeaders(request)
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("teleagent: 请求 %s: %w", path, err)
	}
	defer func() {
		_ = response.Body.Close()
	}()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("teleagent: 读取响应 %s: %w", path, err)
	}
	env, parseErr := parseEnvelope(raw)
	if parseErr != nil {
		message := strings.TrimSpace(string(raw))
		if len(message) > 200 {
			message = message[:200]
		}
		return &APIError{Status: response.StatusCode, Message: message, Path: path}
	}
	if err := checkEnvelope(response.StatusCode, path, env); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("teleagent: 解析响应 %s: %w", path, err)
	}
	return nil
}

func parseEnvelope(raw []byte) (envelope, error) {
	var env envelope
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return env, nil
	}
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return env, err
	}
	return env, nil
}

func checkEnvelope(status int, path string, env envelope) error {
	if status == http.StatusUnauthorized {
		return ErrSessionExpired
	}
	code := rawCode(env.Code)
	message := firstNonEmpty(env.Message, env.Msg)
	if env.Error != nil {
		errorCode := rawCode(env.Error.Code)
		if errorCode != "" && errorCode != "0" {
			if errorCode == cloudAuthExpiredCode {
				return ErrSessionExpired
			}
			return &APIError{Status: status, Code: errorCode, Message: env.Error.Message, Path: path}
		}
		if env.Error.Message != "" {
			message = env.Error.Message
		}
	}
	if status < 200 || status >= 300 {
		if code == cloudAuthExpiredCode {
			return ErrSessionExpired
		}
		return &APIError{Status: status, Code: code, Message: message, Path: path}
	}
	switch code {
	case "", "0", "200":
		return nil
	case cloudAuthExpiredCode:
		return ErrSessionExpired
	}
	return &APIError{Status: status, Code: code, Message: message, Path: path}
}

func rawCode(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if strings.HasPrefix(trimmed, "\"") && strings.HasSuffix(trimmed, "\"") {
		trimmed = strings.Trim(trimmed, "\"")
	}
	return trimmed
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func randomUUID() string {
	var buffer [16]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	buffer[6] = (buffer[6] & 0x0f) | 0x40
	buffer[8] = (buffer[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buffer[0:4], buffer[4:6], buffer[6:8], buffer[8:10], buffer[10:16])
}
