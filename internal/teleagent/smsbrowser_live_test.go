//go:build windows

package teleagent

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveSMSBrowserLogin 走一遍真实浏览器流程（会真的发短信），默认不跑。
//
//	TELEAGENT_LIVE_SMS_BROWSER=1 TELEAGENT_LIVE_PHONE=13800000000 go test -run TestLiveSMSBrowserLogin -v ./internal/teleagent/
func TestLiveSMSBrowserLogin(t *testing.T) {
	if os.Getenv("TELEAGENT_LIVE_SMS_BROWSER") != "1" {
		t.Skip("设置 TELEAGENT_LIVE_SMS_BROWSER=1 才跑")
	}
	phone := os.Getenv("TELEAGENT_LIVE_PHONE")
	if phone == "" {
		t.Skip("需要 TELEAGENT_LIVE_PHONE")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	login, err := StartSMSBrowserLogin(ctx, SMSBrowserOptions{
		Phone:      phone,
		OnProgress: func(message string) { t.Log(message) },
	})
	if err != nil {
		t.Fatalf("启动浏览器流程: %v", err)
	}
	defer login.Close()
	t.Logf("短信应已发送，手机号 %s", login.Phone())

	if err := login.SubmitCode(ctx, "000000"); err != nil {
		t.Fatalf("提交验证码: %v", err)
	}
	time.Sleep(6 * time.Second)
	text, err := evalString(ctx, login.frame, "document.body.innerText")
	if err != nil {
		t.Fatalf("读取页面文本: %v", err)
	}
	t.Logf("提交后页面文本：%s", shortText(text, 300))
	parent, err := evalString(ctx, login.page, "location.href")
	t.Logf("父页面地址：%s（%v）", parent, err)
}
