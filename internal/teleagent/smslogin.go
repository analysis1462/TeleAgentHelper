package teleagent

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// 短信验证码登录：授权链路与账号密码登录一致，但登录动作发生在天翼账号
// （open.e.189.cn）的登录框里：先由统一认证签发一次性登录地址，再取 189 的
// RSA 公钥，最后把手机号与验证码加密提交。
const (
	// openLoginURLPath 由统一认证生成带签名的 189 登录地址。
	openLoginURLPath = "/upms/public/auth/getOpenLoginUrl"
	// s189Origin 是天翼账号登录框的源站。
	s189Origin = "https://open.e.189.cn"
	// s189TransferOrigin 把回调包一层再交给 189 登录框。
	s189TransferOrigin = "https://www.teleai.com.cn/transfer"

	s189AppConfPath     = "/api/logbox/oauth2/appConf.do"
	s189EncryptConfPath = "/api/logbox/config/encryptConf.do"
	s189NeedCaptchaPath = "/api/logbox/oauth2/smsNeedcaptcha.do"
	s189SendCodePath    = "/api/logbox/oauth2/web/sendSmsCode.do"
	s189LoginSubmitPath = "/api/logbox/oauth2/loginSubmit.do"

	// s189CaptchaFree 表示这次发送短信不需要图形验证码。
	s189CaptchaFree = "1"
	// smsLoginResultOK 是 189 侧业务成功的 result 值。
	smsLoginResultOK = 0
	// smsRSASize 是 189 侧 RSA-1024 密文转 hex 后的固定长度。
	smsRSASize = 256

	// 189 登录成功后，平台侧还要把一次性票据换成会话，再签发给客户端回调。
	openLoginCallbackPath  = "/upms/public/auth/openLoginCallbackV2"
	userAccountListPath    = "/upms/public/auth/userAccountList"
	chooseAccountLoginPath = "/upms/public/auth/chooseUserAccountLogin"
	// agentCallbackPath 是 TeleAgent 拿 x-token 的回调地址。
	agentCallbackPath = "/superCowork/sapi/cowork/auth?appVersion=&source=3"
)

// SMSCodeOptions 是发送登录短信验证码的参数。
type SMSCodeOptions struct {
	Phone      string
	HTTPClient *http.Client
	UserAgent  string
	Now        func() time.Time
	// OnProgress 可选，回报登录过程中的关键步骤：例如纯 HTTP 换票失败、
	// 需要退回浏览器完成换票。界面据此把过程写进日志。
	OnProgress func(message string)
}

// SMSLogin 是短信登录的中间态：先用 SendLoginSMSCode 拿到它，向用户索取验证码，
// 再用 CompleteLoginWithSMSCode 完成登录。两次调用必须复用同一个 SMSLogin，
// 因为它持有本轮登录的 cookie 容器与 189 页面参数。
type SMSLogin struct {
	Phone       string
	AppID       string
	LT          string
	ReqID       string
	PageKey     string
	AccountType string
	ClientType  string
	IsOAuth2    bool
	ParamID     string
	ReturnURL   string
	State       string
	MailSuffix  string
	Pre         string
	PublicKey   string
	// ClientID / RedirectURI 是统一认证签发给网页端的客户端标识与回调地址，
	// 189 登录成功后要用它们把票据换成平台会话。
	ClientID    string
	RedirectURI string
	// CaptchaType / CaptchaToken / CaptchaValidate 是自动过 189 图形验证码的产物，
	// 发短信与提交验证码时都要带回去。
	CaptchaType     string
	CaptchaToken    string
	CaptchaValidate string
	CreatedAt       time.Time

	encryptedPhone string
	client         *http.Client
	userAgent      string
	referer        string
	progress       func(message string)
}

// report 在配置了进度回调时回报一条过程信息。
func (s *SMSLogin) report(message string) {
	if s == nil || s.progress == nil || strings.TrimSpace(message) == "" {
		return
	}
	s.progress(message)
}

// SendLoginSMSCode 走完「签发登录地址 -> 打开 189 登录框 -> 取公钥 -> 免图形验证码
// 检查 -> 发送短信」，成功返回可用于校验验证码的中间态。
func SendLoginSMSCode(ctx context.Context, options SMSCodeOptions) (*SMSLogin, error) {
	sms, captchaFree, err := prepareSMSLogin(ctx, options)
	if err != nil {
		return nil, err
	}
	// 该号码被要求过图形验证码时，自动取图、算缺口、交卷，拿到 validate 再发短信。
	captchaType, captchaToken, validateCode := "", "", ""
	if !captchaFree {
		sms.report("该号码需要图形验证码，正在自动处理…")
		solution, err := sms.solve189Captcha(ctx)
		if err != nil {
			return nil, err
		}
		captchaType, captchaToken, validateCode = "1", solution.Token, solution.Validate
		sms.CaptchaType, sms.CaptchaToken, sms.CaptchaValidate = captchaType, captchaToken, validateCode
		sms.report("图形验证码已通过，正在发送短信…")
	}
	raw, err := post189Form(ctx, sms.client, s189SendCodePath, sms.referer, sms.userAgent, sms.LT, sms.ReqID, url.Values{
		"version":      {"v2.0"},
		"mobile":       {sms.encryptedPhone},
		"appKey":       {sms.AppID},
		"captchaType":  {captchaType},
		"captchaToken": {captchaToken},
		"validateCode": {validateCode},
	})
	if err != nil {
		return nil, err
	}
	result, err := decode189Result(raw)
	if err != nil {
		return nil, err
	}
	if result.value != smsLoginResultOK {
		return nil, &LoginError{
			Code:    strconv.Itoa(result.value),
			Message: firstNonEmpty(result.message, smsSendCodeText(result.value)),
		}
	}
	return sms, nil
}

// s189LoginBase 是 189 登录框的公共参数：签发登录地址、取 appConf 之后，
// 短信登录与扫码登录都要用到的页面与会话信息。
type s189LoginBase struct {
	AppID       string
	LT          string
	ReqID       string
	PageKey     string
	AccountType string
	ClientType  string
	IsOAuth2    bool
	ParamID     string
	ReturnURL   string
	State       string
	MailSuffix  string
	ClientID    string
	RedirectURI string
	Referer     string
	CreatedAt   time.Time
	Client      *http.Client
	UserAgent   string
	progress    func(message string)
}

// open189LoginBase 走完「签发登录地址 -> 打开 189 登录框 -> appConf」，
// 返回短信与扫码两种登录方式共用的参数。
func open189LoginBase(ctx context.Context, options SMSCodeOptions) (*s189LoginBase, error) {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	userAgent := options.UserAgent
	if userAgent == "" {
		userAgent = defaultUserAgent
	}
	client, err := ensureHTTPClient(options.HTTPClient)
	if err != nil {
		return nil, err
	}

	clientID, redirectURI, loginPageURL, err := resolveLoginFlowFull(ctx, client, userAgent)
	if err != nil {
		return nil, err
	}
	transfer := s189TransferOrigin + "?redirect_uri=" + encodeURIParam(redirectURI) +
		"&client_id=" + encodeURIParam(clientID)
	openURL := LoginOrigin + LoginAPIPrefix + openLoginURLPath +
		"?openType=TYID&redirectUri=" + encodeURIComponentKeepColon(transfer) +
		"&type=1&clientId=" + encodeURIParam(clientID)
	loginURL, err := request189LoginURL(ctx, client, openURL, loginPageURL.String(), userAgent)
	if err != nil {
		return nil, err
	}
	indexURL, err := follow189LoginURL(ctx, client, loginURL, userAgent)
	if err != nil {
		return nil, err
	}
	query := indexURL.Query()
	appID := strings.TrimSpace(query.Get("appId"))
	lt := strings.TrimSpace(query.Get("lt"))
	requestID := strings.TrimSpace(query.Get("reqId"))
	if appID == "" || lt == "" || requestID == "" {
		return nil, errS189SignInvalid
	}

	appConf, err := post189Form(ctx, client, s189AppConfPath, indexURL.String(), userAgent, lt, requestID,
		url.Values{"version": {"2.0"}, "appKey": {appID}})
	if err != nil {
		return nil, err
	}
	conf, err := decodeAppConf(appConf)
	if err != nil {
		return nil, err
	}
	if conf["appKey"] != "" {
		appID = conf["appKey"]
	}

	return &s189LoginBase{
		AppID:       appID,
		LT:          lt,
		ReqID:       requestID,
		PageKey:     conf["pageKey"],
		AccountType: conf["accountType"],
		ClientType:  conf["clientType"],
		IsOAuth2:    strings.EqualFold(conf["isOauth2"], "true"),
		ParamID:     conf["paramId"],
		ReturnURL:   conf["returnUrl"],
		State:       conf["state"],
		MailSuffix:  conf["mailSuffix"],
		ClientID:    clientID,
		RedirectURI: redirectURI,
		Referer:     indexURL.String(),
		CreatedAt:   now(),
		Client:      client,
		UserAgent:   userAgent,
		progress:    options.OnProgress,
	}, nil
}

// prepareSMSLogin 完成发送短信之前的全部准备，并回报该号码是否免图形验证码。
// 单独拆出来是为了在不真正发短信的前提下验证签名与 RSA 加密链路。
func prepareSMSLogin(ctx context.Context, options SMSCodeOptions) (*SMSLogin, bool, error) {
	phone := normalizePhone(options.Phone)
	if !validMainlandPhone(phone) {
		return nil, false, errPhoneRequired
	}
	base, err := open189LoginBase(ctx, options)
	if err != nil {
		return nil, false, err
	}

	encConf, err := post189Form(ctx, base.Client, s189EncryptConfPath, base.Referer, base.UserAgent, "", "",
		url.Values{"appId": {base.AppID}})
	if err != nil {
		return nil, false, err
	}
	pre, publicKey, err := decodeEncryptConf(encConf)
	if err != nil {
		return nil, false, err
	}
	encryptedPhone, err := rsaPasswordMessage(publicKey, pre, phone)
	if err != nil {
		return nil, false, err
	}
	need, err := post189Form(ctx, base.Client, s189NeedCaptchaPath, base.Referer, base.UserAgent, base.LT, base.ReqID,
		url.Values{"mobile": {encryptedPhone}, "appKey": {base.AppID}})
	if err != nil {
		return nil, false, err
	}
	captchaFree := false
	switch strings.TrimSpace(string(need)) {
	case s189CaptchaFree:
		captchaFree = true
	case "0":
	default:
		return nil, false, &LoginError{Code: strings.TrimSpace(string(need)), Message: "短信验证码接口返回异常"}
	}

	return &SMSLogin{
		Phone:          phone,
		AppID:          base.AppID,
		LT:             base.LT,
		ReqID:          base.ReqID,
		PageKey:        base.PageKey,
		AccountType:    base.AccountType,
		ClientType:     base.ClientType,
		IsOAuth2:       base.IsOAuth2,
		ParamID:        base.ParamID,
		ReturnURL:      base.ReturnURL,
		State:          base.State,
		MailSuffix:     base.MailSuffix,
		Pre:            pre,
		PublicKey:      publicKey,
		ClientID:       base.ClientID,
		RedirectURI:    base.RedirectURI,
		CreatedAt:      base.CreatedAt,
		encryptedPhone: encryptedPhone,
		client:         base.Client,
		userAgent:      base.UserAgent,
		referer:        base.Referer,
		progress:       base.progress,
	}, captchaFree, nil
}

// CompleteLoginWithSMSCode 提交验证码完成 189 登录，并跟随回调写入 x-token。
func CompleteLoginWithSMSCode(ctx context.Context, sms *SMSLogin, code string) (Session, error) {
	if sms == nil || sms.client == nil || sms.PublicKey == "" {
		return Session{}, errSMSLoginMissing
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return Session{}, errSMSCodeRequired
	}
	encryptedPhone, err := rsaPasswordMessage(sms.PublicKey, sms.Pre, sms.Phone)
	if err != nil {
		return Session{}, err
	}
	encryptedCode, err := rsaPasswordMessage(sms.PublicKey, sms.Pre, code)
	if err != nil {
		return Session{}, err
	}
	raw, err := post189Form(ctx, sms.client, s189LoginSubmitPath, sms.referer, sms.userAgent, sms.LT, sms.ReqID, url.Values{
		"version":         {"v2.0"},
		"apToken":         {""},
		"appKey":          {sms.AppID},
		"pageKey":         {sms.PageKey},
		"accountType":     {sms.AccountType},
		"userName":        {encryptedPhone},
		"epd":             {encryptedCode},
		"captchaType":     {sms.CaptchaType},
		"validateCode":    {sms.CaptchaValidate},
		"smsValidateCode": {""},
		"captchaToken":    {sms.CaptchaToken},
		"returnUrl":       {url.QueryEscape(sms.ReturnURL)},
		"mailSuffix":      {sms.MailSuffix},
		"dynamicCheck":    {"TRUE"},
		"clientType":      {sms.ClientType},
		"cb_SaveName":     {"1"},
		"isOauth2":        {strconv.FormatBool(sms.IsOAuth2)},
		"state":           {sms.State},
		"paramId":         {sms.ParamID},
	})
	if err != nil {
		return Session{}, err
	}
	result, err := decode189Result(raw)
	if err != nil {
		return Session{}, err
	}
	if result.value != smsLoginResultOK {
		return Session{}, &LoginError{
			Code:    strconv.Itoa(result.value),
			Message: firstNonEmpty(result.message, smsSubmitText(result.value)),
		}
	}
	toURL, err := decode189ToURL(raw)
	if err != nil {
		return Session{}, err
	}
	if toURL == "" {
		return Session{}, errNoRedirect
	}
	// 换票优先交给浏览器：189 回调指向 /transfer 单页应用，x-token 由那个页面
	// 的脚本写入，纯 HTTP 只能拿到 SPA 的 HTML 外壳。浏览器用全新临时 profile，
	// 页面里没有旧登录态，取到的只可能是本次会话。
	browserSession, browserErr := completeSMSTicketInBrowser(ctx, toURL, nil)
	if browserErr == nil {
		sms.report("浏览器换票成功，已取得登录会话。")
		return browserSession, nil
	}
	sms.report("浏览器换票未成功（" + browserErr.Error() + "），改走纯 HTTP 换票…")

	var httpErr error
	if target, exchangeErr := exchange189Ticket(ctx, sms, toURL); exchangeErr == nil {
		if followErr := followLoginRedirect(ctx, sms.client, sms.userAgent, target); followErr == nil {
			var session Session
			if session, httpErr = sessionFromJar(sms.client, time.Now()); httpErr == nil {
				sms.report("纯 HTTP 换票成功，已取得登录会话。")
				return session, nil
			}
		} else {
			httpErr = followErr
		}
	} else {
		httpErr = exchangeErr
	}
	return Session{}, fmt.Errorf("%w（浏览器换票也失败：%v）", httpErr, browserErr)
}

// exchange189Ticket 把 189 登录成功回跳地址里的一次性票据换成平台会话，
// 返回最终要访问的 TeleAgent 回调地址（访问它才会写入 x-token）。
//
// 网页端把这一步交给 /transfer 单页应用完成，它按顺序调用三个接口：
// openLoginCallbackV2（校验 189 票据并写平台会话）-> userAccountList（取主账号）
// -> chooseUserAccountLogin（签发客户端回调地址）。
func exchange189Ticket(ctx context.Context, sms *SMSLogin, toURL string) (string, error) {
	if sms == nil {
		return "", errSMSLoginMissing
	}
	parsed, err := url.Parse(strings.TrimSpace(toURL))
	if err != nil {
		return "", fmt.Errorf("teleagent: 解析登录回跳地址: %w", err)
	}
	query := parsed.Query()
	clientID := firstNonEmpty(strings.TrimSpace(query.Get("client_id")), sms.ClientID)
	redirectURI := firstNonEmpty(strings.TrimSpace(query.Get("redirect_uri")), sms.RedirectURI)
	return exchangeTeleaiTicket(ctx, sms.client, sms.userAgent, sms.referer, clientID, redirectURI, toURL)
}

// exchangeTeleaiTicket 与 exchange189Ticket 相同，但不依赖短信登录的中间态：
// 扫码登录拿到的回跳地址同样走这三个接口换票。
func exchangeTeleaiTicket(ctx context.Context, client *http.Client, userAgent, referer, clientID, redirectURI, toURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(toURL))
	if err != nil {
		return "", fmt.Errorf("teleagent: 解析登录回跳地址: %w", err)
	}
	query := parsed.Query()
	appID := strings.TrimSpace(query.Get("appId"))
	paras := strings.TrimSpace(query.Get("paras"))
	sign := strings.TrimSpace(query.Get("sign"))
	if appID == "" || paras == "" || sign == "" {
		return "", fmt.Errorf("%w（回跳地址 %s）", err189CallbackParams, toURL)
	}
	clientID = firstNonEmpty(strings.TrimSpace(query.Get("client_id")), clientID)
	redirectURI = firstNonEmpty(strings.TrimSpace(query.Get("redirect_uri")), redirectURI)
	if redirectURI == "" {
		redirectURI = DefaultOrigin + agentCallbackPath
	}
	referer = firstNonEmpty(referer, LoginOrigin+"/master-auth")

	callbackURL := LoginOrigin + LoginAPIPrefix + openLoginCallbackPath + "?" + url.Values{
		"openType":    {"TYID"},
		"appId":       {appID},
		"paras":       {paras},
		"sign":        {sign},
		"redirectUri": {redirectURI},
		"clientId":    {clientID},
	}.Encode()
	callbackData, err := getTeleaiJSON(ctx, client, userAgent, referer, callbackURL, openLoginCallbackPath)
	if err != nil {
		return "", err
	}
	if err := checkTeleaiCode(callbackData); err != nil {
		return "", err
	}

	listURL := LoginOrigin + LoginAPIPrefix + userAccountListPath + "?subUser=true"
	listData, err := getTeleaiJSON(ctx, client, userAgent, referer, listURL, userAccountListPath)
	if err != nil {
		return "", err
	}
	if err := checkTeleaiCode(listData); err != nil {
		return "", err
	}
	masterUID := firstAccountID(listData)
	if masterUID == "" {
		masterUID = firstNonEmpty(confString(nestedData(callbackData), "masterUid"),
			confString(nestedData(callbackData), "accountId"),
			confString(nestedData(callbackData), "userId"))
	}
	if masterUID == "" {
		return "", err189AccountMissing
	}

	body, err := json.Marshal(map[string]string{
		"masterUid":      masterUID,
		"redirectUri":    redirectURI,
		"clientId":       clientID,
		"invitationCode": "",
	})
	if err != nil {
		return "", fmt.Errorf("teleagent: 构造账号登录请求: %w", err)
	}
	loginURL := LoginOrigin + LoginAPIPrefix + chooseAccountLoginPath
	loginData, err := postTeleaiJSON(ctx, client, userAgent, referer, loginURL, chooseAccountLoginPath, body)
	if err != nil {
		return "", err
	}
	if err := checkTeleaiCode(loginData); err != nil {
		return "", err
	}
	data := nestedData(loginData)
	target := firstNonEmpty(confString(data, "redirectUri"), confString(data, "redirectUrl"), confString(data, "url"))
	if target == "" {
		return "", errNoRedirect
	}
	return target, nil
}

// getTeleaiJSON 请求平台侧接口并返回原始 JSON。
func getTeleaiJSON(ctx context.Context, client *http.Client, userAgent, referer, target, path string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("teleagent: 构造平台请求: %w", err)
	}
	request.Header.Set("Accept", "application/json, text/plain, */*")
	request.Header.Set("User-Agent", userAgent)
	if referer != "" {
		request.Header.Set("Referer", referer)
	}
	return doTeleaiRequest(client, request, path)
}

// postTeleaiJSON 以 JSON 提交平台侧接口并返回原始 JSON。
func postTeleaiJSON(ctx context.Context, client *http.Client, userAgent, referer, target, path string, body []byte) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("teleagent: 构造平台请求: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/plain, */*")
	request.Header.Set("User-Agent", userAgent)
	if referer != "" {
		request.Header.Set("Referer", referer)
	}
	return doTeleaiRequest(client, request, path)
}

func doTeleaiRequest(client *http.Client, request *http.Request, path string) ([]byte, error) {
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("teleagent: 请求平台接口 %s: %w", path, err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("teleagent: 读取平台响应 %s: %w", path, err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, &APIError{Status: response.StatusCode, Path: path}
	}
	return raw, nil
}

// checkTeleaiCode 校验平台侧统一响应信封的 code。
func checkTeleaiCode(raw []byte) error {
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("teleagent: 解析平台响应: %w", err)
	}
	code := strings.TrimSpace(confString(envelope, "code"))
	if code == accountLoginOK {
		return nil
	}
	message := firstNonEmpty(confString(envelope, "message"), confString(envelope, "msg"))
	return &LoginError{Code: firstNonEmpty(code, "unknown"), Message: firstNonEmpty(message, "平台登录接口返回异常")}
}

// nestedData 取出统一响应信封里的 data 对象（可能是字符串或缺失）。
func nestedData(raw []byte) map[string]any {
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return map[string]any{}
	}
	if data, ok := envelope["data"].(map[string]any); ok {
		return data
	}
	return map[string]any{}
}

// firstAccountID 取出账号列表里的第一个 accountId。
func firstAccountID(raw []byte) string {
	data := nestedData(raw)
	for _, key := range []string{"accounts", "accountList", "list"} {
		rows, ok := data[key].([]any)
		if !ok {
			continue
		}
		for _, row := range rows {
			item, ok := row.(map[string]any)
			if !ok {
				continue
			}
			if value := firstNonEmpty(confString(item, "accountId"), confString(item, "masterUid"), confString(item, "id")); value != "" {
				return value
			}
		}
	}
	return firstNonEmpty(confString(data, "masterUid"), confString(data, "accountId"), confString(data, "userId"))
}

type s189Result struct {
	value   int
	message string
}

func decode189Result(raw []byte) (s189Result, error) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return s189Result{}, fmt.Errorf("teleagent: 解析 189 业务响应: %w", err)
	}
	return s189Result{value: confInt(payload, "result"), message: strings.TrimSpace(confString(payload, "msg"))}, nil
}

func decode189ToURL(raw []byte) (string, error) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("teleagent: 解析 189 登录响应: %w", err)
	}
	return strings.TrimSpace(confString(payload, "toUrl")), nil
}

// resolveLoginFlowFull 与 resolveLoginFlow 相同，但额外返回最终落地的统一认证页面，
// 后续接口要拿它当 Referer。
func resolveLoginFlowFull(ctx context.Context, client *http.Client, userAgent string) (string, string, *url.URL, error) {
	target := DefaultOrigin + "/superCowork/sapi/cowork/auth?source=" + authSourceWeb
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", "", nil, fmt.Errorf("teleagent: 构造授权请求: %w", err)
	}
	request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	request.Header.Set("User-Agent", userAgent)
	response, err := client.Do(request)
	if err != nil {
		return "", "", nil, fmt.Errorf("teleagent: 请求统一认证: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
	if response.StatusCode != http.StatusOK {
		return "", "", nil, &APIError{Status: response.StatusCode, Path: "/superCowork/sapi/cowork/auth"}
	}
	final := response.Request.URL
	query := final.Query()
	clientID := strings.TrimSpace(query.Get("client_id"))
	redirectURI := strings.TrimSpace(query.Get("redirect_uri"))
	if clientID == "" || redirectURI == "" {
		return "", "", nil, errLoginFlowMissing
	}
	return clientID, redirectURI, final, nil
}

// request189LoginURL 让统一认证签发带签名的 189 登录地址。
func request189LoginURL(ctx context.Context, client *http.Client, openURL, referer, userAgent string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, openURL, nil)
	if err != nil {
		return "", fmt.Errorf("teleagent: 构造 189 登录地址请求: %w", err)
	}
	request.Header.Set("Accept", "application/json, text/plain, */*")
	request.Header.Set("Referer", referer)
	request.Header.Set("User-Agent", userAgent)
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("teleagent: 获取 189 登录地址: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("teleagent: 读取 189 登录地址响应: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return "", &APIError{Status: response.StatusCode, Path: openLoginURLPath}
	}
	var env loginEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("teleagent: 解析 189 登录地址响应: %w", err)
	}
	if env.Code != accountLoginOK {
		return "", &LoginError{Code: env.Code, Message: env.Message}
	}
	var target string
	if err := json.Unmarshal(env.Data, &target); err != nil {
		return "", fmt.Errorf("teleagent: 解析 189 登录地址: %w", err)
	}
	if strings.TrimSpace(target) == "" {
		return "", errLoginFlowMissing
	}
	return target, nil
}

// follow189LoginURL 打开登录地址，跟到真正的登录框页面（URL 上带 appId/lt/reqId）。
func follow189LoginURL(ctx context.Context, client *http.Client, target, userAgent string) (*url.URL, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("teleagent: 构造 189 登录框请求: %w", err)
	}
	request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	request.Header.Set("User-Agent", userAgent)
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("teleagent: 打开 189 登录框: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
	if response.StatusCode != http.StatusOK {
		return nil, &APIError{Status: response.StatusCode, Path: "/api/logbox/oauth2/separate/unifyAccountLogin.do"}
	}
	return response.Request.URL, nil
}

func post189Form(ctx context.Context, client *http.Client, path, referer, userAgent, lt, requestID string, form url.Values) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s189Origin+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("teleagent: 构造 189 请求: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Origin", s189Origin)
	request.Header.Set("User-Agent", userAgent)
	if referer != "" {
		request.Header.Set("Referer", referer)
	}
	if lt != "" {
		request.Header.Set("lt", lt)
	}
	if requestID != "" {
		request.Header.Set("reqId", requestID)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("teleagent: 请求 189 接口 %s: %w", path, err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("teleagent: 读取 189 响应 %s: %w", path, err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, &APIError{Status: response.StatusCode, Path: path}
	}
	return raw, nil
}

func decodeAppConf(raw []byte) (map[string]string, error) {
	data, err := decode189Data(raw)
	if err != nil {
		return nil, err
	}
	fields := map[string]string{}
	for _, key := range []string{"appKey", "pageKey", "accountType", "clientType", "isOauth2", "paramId", "returnUrl", "state", "mailSuffix"} {
		fields[key] = confString(data, key)
	}
	return fields, nil
}

func decodeEncryptConf(raw []byte) (string, string, error) {
	data, err := decode189Data(raw)
	if err != nil {
		return "", "", err
	}
	publicKey := strings.TrimSpace(confString(data, "pubKey"))
	if publicKey == "" {
		return "", "", errS189SignInvalid
	}
	return confString(data, "pre"), publicKey, nil
}

func decode189Data(raw []byte) (map[string]any, error) {
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("teleagent: 解析 189 响应: %w", err)
	}
	data, ok := envelope["data"].(map[string]any)
	if !ok || len(data) == 0 {
		return nil, errS189SignInvalid
	}
	return data, nil
}

// rsaPasswordMessage 用 189 的 RSA 公钥做 PKCS#1 v1.5 加密，输出 {NRP}+小写 hex。
func rsaPasswordMessage(publicKeyB64, prefix, plaintext string) (string, error) {
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(publicKeyB64))
	if err != nil {
		return "", fmt.Errorf("teleagent: 解码 189 公钥: %w", err)
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return "", fmt.Errorf("teleagent: 解析 189 公钥: %w", err)
	}
	key, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return "", errS189SignInvalid
	}
	encrypted, err := rsa.EncryptPKCS1v15(rand.Reader, key, []byte(plaintext))
	if err != nil {
		return "", fmt.Errorf("teleagent: 加密 189 字段: %w", err)
	}
	hexed := hex.EncodeToString(encrypted)
	if len(hexed) < smsRSASize {
		hexed = strings.Repeat("0", smsRSASize-len(hexed)) + hexed
	}
	return prefix + hexed, nil
}

// encodeURIComponentKeepColon 复刻网页端「encodeURIComponent 后放过冒号」的写法。
func encodeURIComponentKeepColon(value string) string {
	var builder strings.Builder
	builder.Grow(len(value) * 3)
	for index := 0; index < len(value); index++ {
		char := value[index]
		switch {
		case char >= 'A' && char <= 'Z', char >= 'a' && char <= 'z', char >= '0' && char <= '9':
			builder.WriteByte(char)
		case char == '-', char == '_', char == '.', char == '~', char == ':':
			builder.WriteByte(char)
		default:
			builder.WriteByte('%')
			builder.WriteByte(upperHexDigits[char>>4])
			builder.WriteByte(upperHexDigits[char&0x0f])
		}
	}
	return builder.String()
}

const upperHexDigits = "0123456789ABCDEF"

// encodeURIParam 与 encodeURIComponent 等价（QueryEscape 会把空格写成 +，先换回来）。
func encodeURIParam(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}

func confString(values map[string]any, key string) string {
	value, ok := values[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case json.Number:
		return typed.String()
	}
	return ""
}

func confInt(values map[string]any, key string) int {
	switch typed := values[key].(type) {
	case float64:
		return int(typed)
	case json.Number:
		parsed, err := typed.Int64()
		if err == nil {
			return int(parsed)
		}
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		if err == nil {
			return parsed
		}
	}
	return 0
}

func ensureHTTPClient(client *http.Client) (*http.Client, error) {
	if client == nil {
		jar, err := cookiejar.New(nil)
		if err != nil {
			return nil, fmt.Errorf("teleagent: 初始化 cookie 容器: %w", err)
		}
		return &http.Client{Jar: jar, Timeout: defaultTimeout}, nil
	}
	if client.Jar == nil {
		jar, err := cookiejar.New(nil)
		if err != nil {
			return nil, fmt.Errorf("teleagent: 初始化 cookie 容器: %w", err)
		}
		client.Jar = jar
	}
	return client, nil
}

func normalizePhone(phone string) string {
	return strings.TrimSpace(strings.ReplaceAll(phone, " ", ""))
}

// validMainlandPhone 校验大陆手机号（189 登录框只接受 11 位大陆号码）。
func validMainlandPhone(phone string) bool {
	if len(phone) != 11 || phone[0] != '1' {
		return false
	}
	for index := 0; index < len(phone); index++ {
		if phone[index] < '0' || phone[index] > '9' {
			return false
		}
	}
	return true
}

func smsSendCodeText(result int) string {
	switch result {
	case 20104, 51129:
		return "验证码发送次数过多，请 24 小时后再试"
	case 20107:
		return "手机号无效"
	case -10320:
		return "该账号不支持短信验证登录"
	case -20099:
		return "登录页面已过期，请重试"
	}
	return "发送验证码失败"
}

func smsSubmitText(result int) string {
	switch result {
	case -133:
		return "需要短信验证码"
	case -134:
		return "需要账号密码"
	case -151:
		return "账号已冻结"
	case -20099:
		return "登录页面已过期，请重试"
	case -30199:
		return "当前 IP 已被限制"
	case 20104, 51129:
		return "验证码错误次数过多，请重新获取"
	}
	return "验证码不正确或已过期"
}
