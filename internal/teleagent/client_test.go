package teleagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := NewClient(ClientOptions{
		Origin:  server.URL,
		Session: Session{Token: "tenantA:" + testJWT, DeviceID: "device-1"},
		Nonce:   func() string { return "nonce-1" },
	})
	return client, server
}

func TestClientSendsProtocolHeaders(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		checks := map[string]string{
			"X-Token":                "tenantA:" + testJWT,
			"X-App-Version":          DefaultAppVersion,
			"X-Os-Type":              "web",
			"X-Client-Platform":      "web",
			"X-Superagent-Device-Id": "device-1",
			"X-Nonce":                "nonce-1",
			"X-Channel-Id":           "",
		}
		for key, want := range checks {
			if got := r.Header.Get(key); got != want {
				t.Errorf("头部 %s = %q，期望 %q", key, got, want)
			}
		}
		if r.Header.Get("X-Timestamp") == "" {
			t.Error("缺少 X-Timestamp")
		}
		_, _ = w.Write([]byte("{\"code\":200,\"data\":{\"status\":\"ok\"}}"))
	})
	if _, err := client.ServiceStatus(context.Background()); err != nil {
		t.Fatalf("ServiceStatus 失败: %v", err)
	}
}

func TestClientTreats401AsExpired(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("{\"message\":\"unauthorized\"}"))
	})
	_, err := client.UserInfo(context.Background())
	if !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("期望 ErrSessionExpired，得到 %v", err)
	}
}

func TestClientTreatsCloudCodeAsExpired(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{\"error\":{\"code\":4001,\"message\":\"云端登录已失效\"}}"))
	})
	_, err := client.Instances(context.Background())
	if !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("期望 ErrSessionExpired，得到 %v", err)
	}
}

func TestClientSurfacesAPIError(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{\"code\":500,\"msg\":\"boom\"}"))
	})
	_, err := client.UserCenter(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("期望 *APIError，得到 %v", err)
	}
	if apiErr.Code != "500" || apiErr.Message != "boom" {
		t.Fatalf("APIError 字段错误: %+v", apiErr)
	}
}

func TestClientParsesNumericErrorCodeZero(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{\"error\":{\"code\":0,\"message\":\"\"},\"data\":[{\"id\":\"i-1\",\"status\":100}]}"))
	})
	instances, err := client.Instances(context.Background())
	if err != nil {
		t.Fatalf("Instances 失败: %v", err)
	}
	if len(instances) != 1 || !instances[0].Running() {
		body, _ := json.Marshal(instances)
		t.Fatalf("实例解析错误: %s", body)
	}
}
