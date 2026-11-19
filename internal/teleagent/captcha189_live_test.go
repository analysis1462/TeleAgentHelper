package teleagent

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"
)

func TestLiveS189Captcha(t *testing.T) {
	if os.Getenv("TELEAGENT_LIVE_CAPTCHA") != "1" {
		t.Skip("设置 TELEAGENT_LIVE_CAPTCHA=1 才会访问真实接口")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	sms, captchaFree, err := prepareSMSLogin(ctx, SMSCodeOptions{
		Phone:      os.Getenv("TELEAGENT_LIVE_PHONE"),
		OnProgress: func(message string) { t.Logf("[progress] %s", message) },
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	pageURL, _ := url.Parse(sms.referer)
	t.Logf("appId=%s captchaFree=%v 起始 cookie=%d", sms.AppID, captchaFree, len(sms.client.Jar.Cookies(pageURL)))
	// 先按网页端行为打开一次登录页，确保 189 会话 cookie 就位。
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sms.referer, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	request.Header.Set("User-Agent", sms.userAgent)
	request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	response, err := sms.client.Do(request)
	if err != nil {
		t.Fatalf("open page: %v", err)
	}
	_ = response.Body.Close()
	t.Logf("打开登录页后 http=%d cookie=%d", response.StatusCode, len(sms.client.Jar.Cookies(pageURL)))
	for _, cookie := range sms.client.Jar.Cookies(pageURL) {
		t.Logf("cookie %s=%s", cookie.Name, cookie.Value)
	}
	solution, err := sms.solve189Captcha(ctx)
	if err != nil {
		t.Logf("solve 结果：%v", err)
		return
	}
	t.Logf("solve 成功：token=%s validate=%s", solution.Token, solution.Validate)
	_ = fmt.Sprintf
}
