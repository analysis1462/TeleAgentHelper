package teleagent

import (
	"testing"
	"time"
)

const testJWT = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJ0ZW5hbnQiOiJ0ZW5hbnRBIiwiYWNjb3VudElkIjoiMTAwMDAwMDAwMDAwMDAwMDAwMSIsImFjY291bnROYW1lIjoiZGVtb191c2VyIiwiZXhwIjoyMDAwMDAwMDAwfQ.sig"

func TestNewTokenAddsTenantPrefix(t *testing.T) {
	if got := NewToken(testJWT, "tenantA"); got != "tenantA:"+testJWT {
		t.Fatalf("NewToken = %q", got)
	}
	if got := NewToken("tenantA:"+testJWT, "tenantB"); got != "tenantA:"+testJWT {
		t.Fatalf("NewToken 不应重复加前缀，得到 %q", got)
	}
}

func TestParseTokenMeta(t *testing.T) {
	meta, err := ParseTokenMeta("tenantA:" + testJWT)
	if err != nil {
		t.Fatalf("ParseTokenMeta 失败: %v", err)
	}
	if meta.Tenant != "tenantA" || meta.AccountID != "1000000000000000001" || meta.AccountName != "demo_user" {
		t.Fatalf("meta 解析错误: %+v", meta)
	}
	if !meta.ExpiresAt.Equal(time.Unix(2000000000, 0)) {
		t.Fatalf("exp 解析错误: %v", meta.ExpiresAt)
	}
}

func TestParseTokenMetaRejectsGarbage(t *testing.T) {
	if _, err := ParseTokenMeta("not-a-token"); err == nil {
		t.Fatal("期望解析失败")
	}
}

func TestSessionCookieHeader(t *testing.T) {
	session := Session{Cookies: []Cookie{{Name: "x-token", Value: "a"}, {Name: "operationToken", Value: "b"}}}
	if got := session.CookieHeader(); got != "x-token=a; operationToken=b" {
		t.Fatalf("CookieHeader = %q", got)
	}
	if value, ok := session.Cookie("operationToken"); !ok || value != "b" {
		t.Fatalf("Cookie 查找失败: %q %v", value, ok)
	}
}

func TestSessionValid(t *testing.T) {
	now := time.Unix(1900000000, 0)
	active := Session{Token: testJWT, ExpiresAt: now.Add(time.Hour)}
	if !active.Valid(now) {
		t.Fatal("未过期会话应有效")
	}
	expired := Session{Token: testJWT, ExpiresAt: now.Add(-time.Minute)}
	if expired.Valid(now) {
		t.Fatal("过期会话应无效")
	}
	if (Session{}).Valid(now) {
		t.Fatal("空会话应无效")
	}
}

func TestSessionNormalize(t *testing.T) {
	session := Session{Token: "tenantA:" + testJWT}
	session.Normalize()
	if session.Version != sessionVersion || session.AppVersion != DefaultAppVersion || session.DeviceID == "" {
		t.Fatalf("Normalize 未补齐字段: %+v", session)
	}
	if session.Tenant != "tenantA" || session.AccountID != "1000000000000000001" {
		t.Fatalf("Normalize 未从 token 推导身份: %+v", session)
	}
	if session.ExpiresAt.IsZero() {
		t.Fatal("Normalize 未设置过期时间")
	}
}
