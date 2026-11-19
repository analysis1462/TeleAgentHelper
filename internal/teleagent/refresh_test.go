package teleagent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func refreshTestJWT(t *testing.T, expiry int64) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, err := json.Marshal(map[string]any{
		"tenant":      "tenantA",
		"accountId":   "1000000000000000001",
		"accountName": "demo_user",
		"exp":         expiry,
	})
	if err != nil {
		t.Fatalf("编码载荷: %v", err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestRefreshSessionUpdatesToken(t *testing.T) {
	now := time.Unix(1800000000, 0)
	oldJWT := refreshTestJWT(t, now.Add(24*time.Hour).Unix())
	newJWT := refreshTestJWT(t, now.Add(7*24*time.Hour).Unix())
	absolute := now.Add(90 * 24 * time.Hour).Unix()
	var gotPath, gotTimestamp, gotOS, gotToken, gotDevice string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotTimestamp = r.Header.Get("X-Timestamp")
		gotOS = r.Header.Get("X-OS-Type")
		gotToken = r.Header.Get("X-Token")
		gotDevice = r.Header.Get("X-SuperAgent-Device-Id")
		if r.Method != http.MethodPost {
			t.Errorf("续期应使用 POST，得到 %s", r.Method)
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"` + newJWT + `","expiresIn":604799,"absoluteExpiresAt":` + strconv.FormatInt(absolute, 10) + `}}`))
	}))
	defer server.Close()

	client := NewClient(ClientOptions{
		Origin:  server.URL,
		Session: Session{Token: "tenantA:" + oldJWT, DeviceID: "dev-1"},
		Now:     func() time.Time { return now },
		Nonce:   func() string { return "nonce-1" },
	})
	session, err := client.RefreshSession(context.Background())
	if err != nil {
		t.Fatalf("RefreshSession 失败: %v", err)
	}
	if gotPath != tokenRefreshPath {
		t.Fatalf("请求路径 = %q", gotPath)
	}
	if gotTimestamp != strconv.FormatInt(now.Unix(), 10) {
		t.Fatalf("X-Timestamp 应为秒级时间戳，得到 %q", gotTimestamp)
	}
	if gotOS != runtimeOSName() || gotOS == "" {
		t.Fatalf("X-OS-Type = %q", gotOS)
	}
	if gotToken != "tenantA:"+oldJWT {
		t.Fatalf("X-Token 应为旧 token，得到 %q", gotToken)
	}
	if gotDevice != "dev-1" {
		t.Fatalf("X-SuperAgent-Device-Id = %q", gotDevice)
	}
	if session.Token != "tenantA:"+newJWT {
		t.Fatalf("新 token 应保留租户前缀，得到 %q", session.Token)
	}
	wantExpiry := now.Add(604799 * time.Second)
	if !session.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("过期时间 = %v，期望 %v", session.ExpiresAt, wantExpiry)
	}
	if client.Session().Token != session.Token {
		t.Fatalf("客户端未热替换会话: %q", client.Session().Token)
	}
}

func TestRefreshSessionClampsToAbsolute(t *testing.T) {
	now := time.Unix(1800000000, 0)
	absolute := now.Add(3 * 24 * time.Hour).Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"` + refreshTestJWT(t, absolute) + `","expiresIn":604799,"absoluteExpiresAt":` + strconv.FormatInt(absolute, 10) + `}}`))
	}))
	defer server.Close()

	client := NewClient(ClientOptions{
		Origin:  server.URL,
		Session: Session{Token: "tenantA:" + refreshTestJWT(t, now.Add(time.Hour).Unix())},
		Now:     func() time.Time { return now },
	})
	session, err := client.RefreshSession(context.Background())
	if err != nil {
		t.Fatalf("RefreshSession 失败: %v", err)
	}
	if !session.ExpiresAt.Equal(time.Unix(absolute, 0)) {
		t.Fatalf("应取更早的绝对过期时间，得到 %v", session.ExpiresAt)
	}
}

func TestRefreshSessionExpired(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"401"}`))
	}))
	defer server.Close()
	client := NewClient(ClientOptions{Origin: server.URL, Session: Session{Token: "tenantA:" + refreshTestJWT(t, time.Now().Add(time.Hour).Unix())}})
	if _, err := client.RefreshSession(context.Background()); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("期望 ErrSessionExpired，得到 %v", err)
	}
}

func TestRefreshSessionReauthCode(t *testing.T) {
	for _, code := range []string{"40112", "40113", "40116"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"code":"` + code + `","message":"ticket 失效"}`))
		}))
		client := NewClient(ClientOptions{Origin: server.URL, Session: Session{Token: "tenantA:" + refreshTestJWT(t, time.Now().Add(time.Hour).Unix())}})
		_, err := client.RefreshSession(context.Background())
		server.Close()
		if !errors.Is(err, ErrSessionExpired) {
			t.Fatalf("业务码 %s 应判定为会话失效，得到 %v", code, err)
		}
	}
}

func TestRefreshSessionRotatedSession(t *testing.T) {
	// 真机实测：HTTP 401 + {"code":40115,"message":"session not found"}，此时 token 本身仍可调管理接口。
	for _, status := range []int{http.StatusUnauthorized, http.StatusOK} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"code":40115,"message":"session not found"}`))
		}))
		client := NewClient(ClientOptions{Origin: server.URL, Session: Session{Token: "tenantA:" + refreshTestJWT(t, time.Now().Add(24*time.Hour).Unix())}})
		_, err := client.RefreshSession(context.Background())
		server.Close()
		if !errors.Is(err, ErrRefreshUnavailable) {
			t.Fatalf("status=%d 期望 ErrRefreshUnavailable，得到 %v", status, err)
		}
		if errors.Is(err, ErrSessionExpired) {
			t.Fatalf("status=%d 不应判定为会话失效", status)
		}
	}
}

func TestRefreshSessionRequiresToken(t *testing.T) {
	client := NewClient(ClientOptions{Origin: "http://example.invalid"})
	if _, err := client.RefreshSession(context.Background()); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("空会话应直接报会话失效，得到 %v", err)
	}
}

func TestRefreshExpiry(t *testing.T) {
	if got := refreshExpiry(0); got != 7*24*time.Hour {
		t.Fatalf("缺省有效期 = %v", got)
	}
	if got := refreshExpiry(-5); got != 7*24*time.Hour {
		t.Fatalf("负有效期 = %v", got)
	}
	if got := refreshExpiry(3600); got != time.Hour {
		t.Fatalf("有效期 = %v", got)
	}
	if got := refreshExpiry(int64(365 * 24 * 3600)); got != refreshAbsoluteLimit {
		t.Fatalf("超长有效期应被截断，得到 %v", got)
	}
}
