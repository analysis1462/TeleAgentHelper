package teleagent

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestValidMainlandPhone(t *testing.T) {
	valid := []string{"13800000000", "19912345678"}
	for _, phone := range valid {
		if !validMainlandPhone(phone) {
			t.Fatalf("手机号 %q 应被判为合法", phone)
		}
	}
	invalid := []string{"", "1380000000", "138000000000", "23800000000", "1380000000a", "+8613800000000"}
	for _, phone := range invalid {
		if validMainlandPhone(phone) {
			t.Fatalf("手机号 %q 应被判为非法", phone)
		}
	}
	if got := normalizePhone(" 138 0000 0000 "); got != "13800000000" {
		t.Fatalf("normalizePhone = %q", got)
	}
	if !validMainlandPhone(normalizePhone("138 0000 0000")) {
		t.Fatal("去掉空格后的手机号应判为合法")
	}
}

func TestEncodeURIComponentKeepColon(t *testing.T) {
	cases := map[string]string{
		"https://www.teleai.com.cn/transfer?redirect_uri=x&client_id=y": "https:%2F%2Fwww.teleai.com.cn%2Ftransfer%3Fredirect_uri%3Dx%26client_id%3Dy",
		"a:b c@d":    "a:b%20c%40d",
		"AZaz09-_.~": "AZaz09-_.~",
		"中文":         "%E4%B8%AD%E6%96%87",
	}
	for input, want := range cases {
		if got := encodeURIComponentKeepColon(input); got != want {
			t.Fatalf("encodeURIComponentKeepColon(%q) = %q, 期望 %q", input, got, want)
		}
	}
}

func TestEncodeURIParamMatchesQueryEscape(t *testing.T) {
	input := "https://agent.teleai.com.cn/superCowork/sapi/cowork/auth?appVersion=&source=3"
	got := encodeURIParam(input)
	if strings.Contains(got, "+") {
		t.Fatalf("encodeURIParam 不应出现加号: %q", got)
	}
	if !strings.HasPrefix(got, "https%3A%2F%2Fagent.teleai.com.cn%2FsuperCowork%2Fsapi%2Fcowork%2Fauth%3F") {
		t.Fatalf("encodeURIParam 结果异常: %q", got)
	}
}

func TestRSAPasswordMessageRoundTrip(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("生成 RSA 密钥失败: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("导出公钥失败: %v", err)
	}
	publicKey := base64.StdEncoding.EncodeToString(der)

	encrypted, err := rsaPasswordMessage(publicKey, "{NRP}", "13800000000")
	if err != nil {
		t.Fatalf("加密手机号失败: %v", err)
	}
	if !strings.HasPrefix(encrypted, "{NRP}") {
		t.Fatalf("密文缺少前缀: %q", encrypted)
	}
	hexed := strings.TrimPrefix(encrypted, "{NRP}")
	if len(hexed) != smsRSASize {
		t.Fatalf("密文长度 = %d, 期望 %d", len(hexed), smsRSASize)
	}
	if strings.ToLower(hexed) != hexed {
		t.Fatalf("密文应为小写 hex: %q", hexed)
	}
	raw, err := hex.DecodeString(hexed)
	if err != nil {
		t.Fatalf("密文不是合法 hex: %v", err)
	}
	plain, err := rsa.DecryptPKCS1v15(nil, key, raw)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if string(plain) != "13800000000" {
		t.Fatalf("解密结果 = %q", plain)
	}
}

func TestConfHelpers(t *testing.T) {
	payload := map[string]any{
		"name":   "normal",
		"flag":   true,
		"number": float64(42),
	}
	if got := confString(payload, "name"); got != "normal" {
		t.Fatalf("confString(name) = %q", got)
	}
	if got := confString(payload, "flag"); got != "true" {
		t.Fatalf("confString(flag) = %q", got)
	}
	if got := confString(payload, "missing"); got != "" {
		t.Fatalf("confString(missing) = %q", got)
	}
	if got := confInt(payload, "number"); got != 42 {
		t.Fatalf("confInt(number) = %d", got)
	}
	if got := confInt(payload, "missing"); got != 0 {
		t.Fatalf("confInt(missing) = %d", got)
	}
}

func TestDecodeAppConfAndEnvelope(t *testing.T) {
	raw := []byte(`{"result":0,"msg":"","data":{"appKey":"0123456789","pageKey":"normal","accountType":"02","clientType":"10010","isOauth2":true,"paramId":"abc","returnUrl":"https://www.teleai.com.cn/transfer?client_id=x","state":""}}`)
	conf, err := decodeAppConf(raw)
	if err != nil {
		t.Fatalf("解析 appConf 失败: %v", err)
	}
	if conf["appKey"] != "0123456789" || conf["pageKey"] != "normal" || conf["accountType"] != "02" {
		t.Fatalf("appConf 字段解析异常: %+v", conf)
	}
	if !strings.EqualFold(conf["isOauth2"], "true") {
		t.Fatalf("isOauth2 解析异常: %q", conf["isOauth2"])
	}

	result, err := decode189Result(raw)
	if err != nil {
		t.Fatalf("解析 result 失败: %v", err)
	}
	if result.value != smsLoginResultOK || result.message != "" {
		t.Fatalf("result 解析异常: %+v", result)
	}

	bad := []byte(`{"result":20104,"msg":"验证码发送次数过多"}`)
	limited, err := decode189Result(bad)
	if err != nil {
		t.Fatalf("解析错误响应失败: %v", err)
	}
	if limited.value != 20104 || !strings.Contains(limited.message, "次数过多") {
		t.Fatalf("错误响应解析异常: %+v", limited)
	}
	if _, err := decode189Data(bad); err == nil {
		t.Fatal("缺少 data 时应报错")
	}

	toURL, err := decode189ToURL([]byte(`{"result":0,"toUrl":"https://open.e.189.cn/next"}`))
	if err != nil || toURL != "https://open.e.189.cn/next" {
		t.Fatalf("decode189ToURL = %q, %v", toURL, err)
	}
}

func TestSendLoginSMSCodeRejectsBadPhone(t *testing.T) {
	if _, err := SendLoginSMSCode(context.Background(), SMSCodeOptions{Phone: "12345"}); err != errPhoneRequired {
		t.Fatalf("错误手机号应返回 errPhoneRequired, 实际 %v", err)
	}
}

func TestCompleteLoginWithSMSCodeRequiresState(t *testing.T) {
	if _, err := CompleteLoginWithSMSCode(context.Background(), nil, "123456"); err != errSMSLoginMissing {
		t.Fatalf("缺失中间态应返回 errSMSLoginMissing, 实际 %v", err)
	}
	state := &SMSLogin{Phone: "13800000000", PublicKey: "x", client: nil}
	if _, err := CompleteLoginWithSMSCode(context.Background(), state, "123456"); err != errSMSLoginMissing {
		t.Fatalf("缺少 client 应返回 errSMSLoginMissing, 实际 %v", err)
	}
}

func TestSMSErrorTexts(t *testing.T) {
	if got := smsSendCodeText(20104); !strings.Contains(got, "24 小时") {
		t.Fatalf("smsSendCodeText(20104) = %q", got)
	}
	if got := smsSubmitText(-20099); !strings.Contains(got, "过期") {
		t.Fatalf("smsSubmitText(-20099) = %q", got)
	}
}

func TestExchange189TicketRejectsMissingParams(t *testing.T) {
	sms := &SMSLogin{ClientID: "client", RedirectURI: "https://agent.example/callback"}
	if _, err := exchange189Ticket(context.Background(), sms, "https://www.teleai.com.cn/transfer?code=abc"); err == nil {
		t.Fatal("缺少 appId/paras/sign 时应返回错误")
	} else if !strings.Contains(err.Error(), "票据参数") {
		t.Fatalf("错误信息应提示缺少票据参数，实际 %v", err)
	}
}

func TestFirstAccountID(t *testing.T) {
	raw := []byte(`{"code":"10980001","data":{"accounts":[{"accountId":"A100","accountName":"主账号"}],"onlySubUser":false}}`)
	if got := firstAccountID(raw); got != "A100" {
		t.Fatalf("firstAccountID = %q", got)
	}
	empty := []byte(`{"code":"10980001","data":{"accounts":[]}}`)
	if got := firstAccountID(empty); got != "" {
		t.Fatalf("空列表应返回空串，实际 %q", got)
	}
	fallback := []byte(`{"code":"10980001","data":{"masterUid":"M200"}}`)
	if got := firstAccountID(fallback); got != "M200" {
		t.Fatalf("应回退到 data.masterUid，实际 %q", got)
	}
}

func TestCheckTeleaiCode(t *testing.T) {
	if err := checkTeleaiCode([]byte(`{"code":"10980001","data":{}}`)); err != nil {
		t.Fatalf("成功码不应报错：%v", err)
	}
	var loginErr *LoginError
	if err := checkTeleaiCode([]byte(`{"code":"10980003","message":"系统异常"}`)); err == nil {
		t.Fatal("失败码应报错")
	} else if !errors.As(err, &loginErr) || loginErr.Code != "10980003" || loginErr.Message != "系统异常" {
		t.Fatalf("LoginError 解析不正确：%+v", err)
	}
	if err := checkTeleaiCode([]byte("not json")); err == nil {
		t.Fatal("非 JSON 响应应报错")
	}
}

func TestNestedData(t *testing.T) {
	if data := nestedData([]byte(`{"code":"10980001","data":{"redirectUri":"https://x"}}`)); data["redirectUri"] != "https://x" {
		t.Fatalf("nestedData = %+v", data)
	}
	if data := nestedData([]byte(`{"code":"10980001","data":"string-body"}`)); len(data) != 0 {
		t.Fatalf("字符串 data 应返回空对象，实际 %+v", data)
	}
	if data := nestedData([]byte("garbage")); len(data) != 0 {
		t.Fatalf("非法 JSON 应返回空对象，实际 %+v", data)
	}
}
