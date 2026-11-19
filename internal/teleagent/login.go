package teleagent

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"strings"
	"time"
)

// 账号密码登录走的是天翼统一认证（master-auth）的混合加密握手。
const (
	// LoginOrigin 是统一认证前端源站，接口前缀挂在它的 /teleai-api 下。
	LoginOrigin = "https://www.teleai.com.cn"
	// LoginAPIPrefix 是 upms 公共接口前缀。
	LoginAPIPrefix = "/teleai-api"
	// authSourceWeb 是网页端的 source 枚举值。
	authSourceWeb = "3"

	rsaPublicKeyPath  = "/upms/public/auth/getRsaPublicKey"
	accountNamePath   = "/upms/public/auth/accountNameLogin"
	accountLoginOK    = "10980001"
	accountBadAuth    = "10980039"
	accountServerBusy = "10980005"
)

// PasswordLoginOptions 是账号密码登录参数。
type PasswordLoginOptions struct {
	Username   string
	Password   string
	HTTPClient *http.Client
	UserAgent  string
	Now        func() time.Time
}

// LoginError 描述登录被服务端拒绝的业务错误。
type LoginError struct {
	Code     string
	Message  string
	ErrCount int
}

func (e *LoginError) Error() string {
	message := strings.TrimSpace(e.Message)
	if message == "" {
		message = "登录失败"
	}
	if e.Code == "" {
		return "teleagent: " + message
	}
	return fmt.Sprintf("teleagent: %s（%s）", message, e.Code)
}

type loginEnvelope struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type loginData struct {
	RedirectURI string `json:"redirectUri"`
	ErrCount    int    `json:"errCount"`
}

type rsaKeyData struct {
	KeyID     string `json:"keyId"`
	PublicKey string `json:"publicKey"`
}

// PasswordLogin 用账号密码完成一次无浏览器登录，返回可直接使用的会话。
//
// 流程：跳统一认证 -> 取 RSA 公钥 -> AES-GCM 加密密码 -> 过滑块 -> 提交登录 ->
// 跟随回调写入 x-token cookie。
func PasswordLogin(ctx context.Context, options PasswordLoginOptions) (Session, error) {
	username := strings.TrimSpace(options.Username)
	password := options.Password
	if username == "" || password == "" {
		return Session{}, errCredentialsRequired
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	userAgent := options.UserAgent
	if userAgent == "" {
		userAgent = defaultUserAgent
	}
	client := options.HTTPClient
	if client == nil {
		jar, err := cookiejar.New(nil)
		if err != nil {
			return Session{}, fmt.Errorf("teleagent: 初始化 cookie 容器: %w", err)
		}
		client = &http.Client{Jar: jar, Timeout: defaultTimeout}
	} else if client.Jar == nil {
		jar, err := cookiejar.New(nil)
		if err != nil {
			return Session{}, fmt.Errorf("teleagent: 初始化 cookie 容器: %w", err)
		}
		client.Jar = jar
	}
	api := LoginOrigin + LoginAPIPrefix
	solver := newCaptchaSolver(client, api, DefaultOrigin)

	clientID, redirectURI, err := resolveLoginFlow(ctx, client, userAgent)
	if err != nil {
		return Session{}, err
	}
	ticket, err := solver.ticket(ctx)
	if err != nil {
		return Session{}, err
	}
	keyData, err := fetchRSAKey(ctx, client, api, userAgent)
	if err != nil {
		return Session{}, err
	}
	payload, err := buildLoginPayload(username, password, clientID, redirectURI, keyData, ticket, now())
	if err != nil {
		return Session{}, err
	}
	env, err := postLogin(ctx, client, api, userAgent, payload)
	if err != nil {
		return Session{}, err
	}
	if env.Code != accountLoginOK {
		var data loginData
		_ = json.Unmarshal(env.Data, &data)
		return Session{}, &LoginError{Code: env.Code, Message: env.Message, ErrCount: data.ErrCount}
	}
	var data loginData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return Session{}, fmt.Errorf("teleagent: 解析登录响应: %w", err)
	}
	if strings.TrimSpace(data.RedirectURI) == "" {
		return Session{}, errNoRedirect
	}
	if err := followLoginRedirect(ctx, client, userAgent, data.RedirectURI); err != nil {
		return Session{}, err
	}
	session, err := sessionFromJar(client, now())
	if err != nil {
		return Session{}, err
	}
	return session, nil
}

// resolveLoginFlow 走一遍授权跳转，拿到 client_id 与回调地址。
func resolveLoginFlow(ctx context.Context, client *http.Client, userAgent string) (string, string, error) {
	target := DefaultOrigin + "/superCowork/sapi/cowork/auth?source=" + authSourceWeb
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", "", fmt.Errorf("teleagent: 构造授权请求: %w", err)
	}
	request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	request.Header.Set("User-Agent", userAgent)
	response, err := client.Do(request)
	if err != nil {
		return "", "", fmt.Errorf("teleagent: 请求统一认证: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
	if response.StatusCode != http.StatusOK {
		return "", "", &APIError{Status: response.StatusCode, Path: "/superCowork/sapi/cowork/auth"}
	}
	final := response.Request.URL
	query := final.Query()
	clientID := strings.TrimSpace(query.Get("client_id"))
	redirectURI := strings.TrimSpace(query.Get("redirect_uri"))
	if clientID == "" || redirectURI == "" {
		return "", "", errLoginFlowMissing
	}
	return clientID, redirectURI, nil
}

func fetchRSAKey(ctx context.Context, client *http.Client, api, userAgent string) (rsaKeyData, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, api+rsaPublicKeyPath, nil)
	if err != nil {
		return rsaKeyData{}, fmt.Errorf("teleagent: 构造公钥请求: %w", err)
	}
	request.Header.Set("Accept", "application/json, text/plain, */*")
	request.Header.Set("Origin", LoginOrigin)
	request.Header.Set("Referer", LoginOrigin+"/master-auth")
	request.Header.Set("User-Agent", userAgent)
	response, err := client.Do(request)
	if err != nil {
		return rsaKeyData{}, fmt.Errorf("teleagent: 获取 RSA 公钥: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return rsaKeyData{}, fmt.Errorf("teleagent: 读取公钥响应: %w", err)
	}
	var env loginEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return rsaKeyData{}, fmt.Errorf("teleagent: 解析公钥响应: %w", err)
	}
	if env.Code != accountLoginOK {
		return rsaKeyData{}, &LoginError{Code: env.Code, Message: env.Message}
	}
	var data rsaKeyData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return rsaKeyData{}, fmt.Errorf("teleagent: 解析公钥数据: %w", err)
	}
	if data.KeyID == "" || data.PublicKey == "" {
		return rsaKeyData{}, errLoginFlowMissing
	}
	return data, nil
}

func buildLoginPayload(username, password, clientID, redirectURI string, keyData rsaKeyData, ticket string, now time.Time) (map[string]any, error) {
	der, err := base64.StdEncoding.DecodeString(keyData.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("teleagent: 解码服务端公钥: %w", err)
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("teleagent: 解析服务端公钥: %w", err)
	}
	serverKey, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, errLoginFlowMissing
	}

	aesKey := make([]byte, 16)
	iv := make([]byte, 12)
	if _, err := rand.Read(aesKey); err != nil {
		return nil, fmt.Errorf("teleagent: 生成会话密钥: %w", err)
	}
	if _, err := rand.Read(iv); err != nil {
		return nil, fmt.Errorf("teleagent: 生成初始向量: %w", err)
	}
	timestamp := now.UnixMilli()
	requestID := randomUUID()
	additionalData := "loginName=" + username + "&keyId=" + keyData.KeyID +
		"&timestamp=" + strconv.FormatInt(timestamp, 10) +
		"&requestId=" + requestID + "&clientId=" + clientID

	// 网页端 accountEncrypt 模块对密码原文直接做 AES-GCM 加密（不经 JSON.stringify），
	// 这里若加引号，服务端解密出的是「"密码"」，比对必然失败并返回 10980039。
	ciphertext, tag, err := aesGCMSeal(aesKey, iv, []byte(password), []byte(additionalData))
	if err != nil {
		return nil, err
	}
	blob := make([]byte, 0, len(aesKey)+len(iv))
	blob = append(blob, aesKey...)
	blob = append(blob, iv...)
	encryptedKey, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, serverKey, blob, nil)
	if err != nil {
		return nil, fmt.Errorf("teleagent: 加密会话密钥: %w", err)
	}
	clientKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("teleagent: 生成客户端密钥: %w", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&clientKey.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("teleagent: 编码客户端公钥: %w", err)
	}
	return map[string]any{
		"loginName":          username,
		"keyId":              keyData.KeyID,
		"timestamp":          timestamp,
		"requestId":          requestID,
		"redirectUri":        redirectURI,
		"clientId":           clientID,
		"encryptedKeyIv":     base64.StdEncoding.EncodeToString(encryptedKey),
		"ciphertext":         base64.StdEncoding.EncodeToString(ciphertext),
		"tag":                base64.StdEncoding.EncodeToString(tag),
		"invitationCode":     "",
		"captchaCheckResult": map[string]any{"captchaVerification": ticket},
		"publicKey":          base64.StdEncoding.EncodeToString(publicDER),
	}, nil
}

func postLogin(ctx context.Context, client *http.Client, api, userAgent string, payload map[string]any) (loginEnvelope, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return loginEnvelope{}, fmt.Errorf("teleagent: 编码登录请求: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, api+accountNamePath, bytes.NewReader(encoded))
	if err != nil {
		return loginEnvelope{}, fmt.Errorf("teleagent: 构造登录请求: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/plain, */*")
	request.Header.Set("Origin", LoginOrigin)
	request.Header.Set("Referer", LoginOrigin+"/master-auth")
	request.Header.Set("User-Agent", userAgent)
	response, err := client.Do(request)
	if err != nil {
		return loginEnvelope{}, fmt.Errorf("teleagent: 提交登录: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return loginEnvelope{}, fmt.Errorf("teleagent: 读取登录响应: %w", err)
	}
	var env loginEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return loginEnvelope{}, &APIError{Status: response.StatusCode, Path: accountNamePath}
	}
	if env.Code == accountServerBusy && response.StatusCode >= 500 {
		return loginEnvelope{}, &LoginError{Code: env.Code, Message: firstNonEmpty(env.Message, "服务端繁忙，请稍后再试")}
	}
	return env, nil
}

func followLoginRedirect(ctx context.Context, client *http.Client, userAgent, target string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("teleagent: 构造回调请求: %w", err)
	}
	request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	request.Header.Set("User-Agent", userAgent)
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("teleagent: 跟随登录回调: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
	return nil
}

func sessionFromJar(client *http.Client, now time.Time) (Session, error) {
	session := Session{CapturedAt: now, Cookies: collectPlatformCookies(client)}
	session.Token = tokenFromCookies(session.Cookies)
	if strings.TrimSpace(session.Token) == "" {
		return Session{}, errLoginTokenMissing
	}
	session.Normalize()
	return session, nil
}

// tokenFromCookies 从 cookie 列表里挑出会话令牌：优先已知名字，其次任意
// JWT 形态的值（服务端偶尔会换 cookie 名，靠载荷形态兜底）。
func tokenFromCookies(cookies []Cookie) string {
	for _, name := range []string{"x-token", "X-Token", "xtoken", "operationToken", "user_token", "operToken"} {
		for _, cookie := range cookies {
			if cookie.Name == name && strings.TrimSpace(cookie.Value) != "" {
				return cookie.Value
			}
		}
	}
	for _, cookie := range cookies {
		if looksLikeJWT(cookie.Value) || looksLikeTenantJWT(cookie.Value) {
			return cookie.Value
		}
	}
	return ""
}

// looksLikeTenantJWT 判断值是否形如 "<tenant>:<jwt>"：站点新版会把 JWT 前挂一段
// 租户/会话前缀（例如 user_token 的值），这种形态同样可以直接当会话令牌用。
func looksLikeTenantJWT(value string) bool {
	value = strings.TrimSpace(value)
	index := strings.LastIndex(value, ":")
	if index < 0 || index == len(value)-1 {
		return false
	}
	return looksLikeJWT(value[index+1:])
}

// looksLikeJWT 判断值是否形如 JWT：三段、首段解出来是 JSON 对象。
func looksLikeJWT(value string) bool {
	value = strings.TrimSpace(value)
	parts := strings.Split(value, ".")
	if len(parts) != 3 || len(parts[0]) < 8 {
		return false
	}
	header, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[0], "="))
	if err != nil {
		return false
	}
	header = bytes.TrimSpace(header)
	return len(header) > 1 && header[0] == '{'
}

// encodeJSONString 复刻浏览器 JSON.stringify 的字符串编码：
// 只转义引号、反斜杠与控制字符，保留 < > & 与非 ASCII 原样。
func encodeJSONString(value string) ([]byte, error) {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, char := range value {
		switch char {
		case '"':
			builder.WriteString("\\\"")
		case '\\':
			builder.WriteString("\\\\")
		case '\n':
			builder.WriteString("\\n")
		case '\r':
			builder.WriteString("\\r")
		case '\t':
			builder.WriteString("\\t")
		case '\b':
			builder.WriteString("\\b")
		case '\f':
			builder.WriteString("\\f")
		default:
			if char < 0x20 {
				builder.WriteString(fmt.Sprintf("\\u%04x", char))
				continue
			}
			builder.WriteRune(char)
		}
	}
	builder.WriteByte('"')
	return []byte(builder.String()), nil
}

func aesGCMSeal(key, iv, plaintext, additionalData []byte) ([]byte, []byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, fmt.Errorf("teleagent: 初始化 AES: %w", err)
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, len(iv))
	if err != nil {
		return nil, nil, fmt.Errorf("teleagent: 初始化 GCM: %w", err)
	}
	sealed := gcm.Seal(nil, iv, plaintext, additionalData)
	tagSize := gcm.Overhead()
	return sealed[:len(sealed)-tagSize], sealed[len(sealed)-tagSize:], nil
}
