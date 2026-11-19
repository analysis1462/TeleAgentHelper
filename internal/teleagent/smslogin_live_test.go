package teleagent

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestPrepareSMSLoginLive 真连天翼账号接口验证「签发地址 -> 取公钥 -> RSA 加密 ->
// 免图形验证码检查」链路，停在发送短信之前，不会真的发短信。
// 需要显式开跑：设置 TELEAGENT_LIVE_SMS=1。
func TestPrepareSMSLoginLive(t *testing.T) {
	if os.Getenv("TELEAGENT_LIVE_SMS") == "" {
		t.Skip("设置 TELEAGENT_LIVE_SMS=1 才连真实接口")
	}
	phone := os.Getenv("TELEAGENT_LIVE_PHONE")
	if phone == "" {
		phone = "13800000000"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sms, captchaFree, err := prepareSMSLogin(ctx, SMSCodeOptions{Phone: phone})
	if err != nil {
		t.Fatalf("prepareSMSLogin 失败: %v", err)
	}
	if sms.AppID == "" || sms.LT == "" || sms.ReqID == "" {
		t.Fatalf("189 参数缺失: appId=%q lt=%q reqId=%q", sms.AppID, sms.LT, sms.ReqID)
	}
	if sms.PublicKey == "" || sms.Pre == "" {
		t.Fatalf("RSA 参数缺失: pre=%q pubLen=%d", sms.Pre, len(sms.PublicKey))
	}
	if len(sms.encryptedPhone) != len(sms.Pre)+smsRSASize {
		t.Fatalf("手机号密文长度 = %d, 期望 %d", len(sms.encryptedPhone), len(sms.Pre)+smsRSASize)
	}
	if sms.ReturnURL == "" || sms.ParamID == "" {
		t.Fatalf("登录参数缺失: returnUrl=%q paramId=%q", sms.ReturnURL, sms.ParamID)
	}
	t.Logf("appId=%s pageKey=%s accountType=%s clientType=%s isOauth2=%v paramLen=%d captchaFree=%v",
		sms.AppID, sms.PageKey, sms.AccountType, sms.ClientType, sms.IsOAuth2, len(sms.ParamID), captchaFree)
	if !captchaFree {
		t.Log("该号码发送短信需要图形验证码，流程需要人工介入")
	}
}
