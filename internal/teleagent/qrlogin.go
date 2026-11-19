package teleagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// 扫码登录：189 登录框的「扫码登录」页签不需要任何图形验证码，流程是
// getUUID.do 取二维码 -> image.do 取图片 -> 每 3 秒 qrcodeLoginState.do 轮询。
// 用户用天翼账号 App（小翼管家 / 天翼云盘 / 微信）扫码确认后，轮询返回
// status=0 与 redirectUrl，后者带 appId/paras/sign，可直接复用短信登录的换票链。
const (
	s189GetUUIDPath = "/api/logbox/oauth2/getUUID.do"
	s189QRImagePath = "/api/logbox/oauth2/image.do"
	s189QRStatePath = "/api/logbox/oauth2/qrcodeLoginState.do"

	// qrPollInterval 是前端源码里的轮询间隔；qrLoginTimeout 与其超时一致。
	qrPollInterval = 3 * time.Second
	qrLoginTimeout = 4 * time.Minute

	// 189 扫码状态码。
	qrStatusOK          = 0
	qrStatusWaiting     = -106
	qrStatusScanned     = -11002
	qrStatusPassword    = -134
	qrStatusExpired     = -11001
	qrStatusPageExpired = -20099

	// qrDateRandomBase 是前端 date 字段尾数的取值范围：Math.floor(24*Math.random())。
	qrDateRandomBase = 24
)

// QRLoginOptions 是获取扫码登录二维码的参数。
type QRLoginOptions struct {
	HTTPClient *http.Client
	UserAgent  string
	Now        func() time.Time
	// OnProgress 可选，回报扫码状态变化（已扫描、已过期刷新、换票中）。
	OnProgress func(message string)
}

// QRLogin 是一次扫码登录的中间态：Image 是二维码 JPEG 原始字节，
// 可直接交给界面显示；Wait 轮询到用户确认后换票并返回会话。
type QRLogin struct {
	// Image 是二维码图片（139x139 JPEG）。
	Image []byte
	// UUID / EncryUUID / EncodeUUID 是 189 侧的三个二维码标识，轮询时都要带。
	UUID        string
	EncryUUID   string
	EncodeUUID  string
	AppID       string
	ClientID    string
	RedirectURI string
	// ExpiresAt 是当前二维码的过期时间；Refresh 之后会顺延。
	ExpiresAt time.Time

	base     *s189LoginBase
	now      func() time.Time
	progress func(message string)
}

// report 在配置了进度回调时回报一条过程信息。
func (q *QRLogin) report(message string) {
	if q == nil || q.progress == nil || strings.TrimSpace(message) == "" {
		return
	}
	q.progress(message)
}

// QRLoginTimeout 返回单张二维码的默认等待时长。
func QRLoginTimeout() time.Duration {
	return qrLoginTimeout
}

// StartQRLogin 走完 189 登录地址与 appConf，再取一张二维码图片。
func StartQRLogin(ctx context.Context, options QRLoginOptions) (*QRLogin, error) {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	base, err := open189LoginBase(ctx, SMSCodeOptions{
		HTTPClient: options.HTTPClient,
		UserAgent:  options.UserAgent,
		Now:        now,
		OnProgress: options.OnProgress,
	})
	if err != nil {
		return nil, err
	}
	q := &QRLogin{
		AppID:       base.AppID,
		ClientID:    base.ClientID,
		RedirectURI: base.RedirectURI,
		base:        base,
		now:         now,
		progress:    options.OnProgress,
	}
	if err := q.Refresh(ctx); err != nil {
		return nil, err
	}
	return q, nil
}

// Refresh 重新取一个二维码，供过期后刷新或用户手动点「刷新」使用。
func (q *QRLogin) Refresh(ctx context.Context) error {
	if q == nil || q.base == nil {
		return errQRLoginMissing
	}
	raw, err := post189Form(ctx, q.base.Client, s189GetUUIDPath, q.base.Referer, q.base.UserAgent, q.base.LT, q.base.ReqID,
		url.Values{"appId": {q.base.AppID}})
	if err != nil {
		return err
	}
	payload, err := decodeQRUUID(raw)
	if err != nil {
		return err
	}
	image, err := q.fetchImage(ctx, payload.EncodeUUID)
	if err != nil {
		return err
	}
	q.UUID, q.EncryUUID, q.EncodeUUID = payload.UUID, payload.EncryUUID, payload.EncodeUUID
	q.Image = image
	q.ExpiresAt = q.now().Add(qrLoginTimeout)
	return nil
}

// fetchImage 取二维码图片；encodeuuid 已由 189 做过一次编码，拼接时不能再转义。
func (q *QRLogin) fetchImage(ctx context.Context, encodeUUID string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, qrImageURL(q.base, encodeUUID), nil)
	if err != nil {
		return nil, fmt.Errorf("teleagent: 构造二维码请求: %w", err)
	}
	request.Header.Set("Accept", "image/avif,image/webp,image/apng,image/*,*/*;q=0.8")
	request.Header.Set("User-Agent", q.base.UserAgent)
	if q.base.Referer != "" {
		request.Header.Set("Referer", q.base.Referer)
	}
	response, err := q.base.Client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("teleagent: 请求二维码图片: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("teleagent: 读取二维码图片: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, &APIError{Status: response.StatusCode, Path: s189QRImagePath}
	}
	if !looksLikeImage(raw) {
		return nil, errQRCodeMissing
	}
	return raw, nil
}

// Wait 每 3 秒轮询一次扫码状态，直到用户确认、调用方取消或 4 分钟超时。
// 二维码过期会自动换一张，按钮与状态由界面按 OnProgress 更新。
func (q *QRLogin) Wait(ctx context.Context) (Session, error) {
	if q == nil || q.base == nil {
		return Session{}, errQRLoginMissing
	}
	deadline := q.now().Add(qrLoginTimeout)
	lastStatus := 1
	for {
		if err := ctx.Err(); err != nil {
			return Session{}, err
		}
		if q.now().After(deadline) {
			return Session{}, &LoginError{Code: "timeout", Message: "扫码登录超时，请刷新二维码后重试"}
		}
		status, redirect, err := q.pollOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return Session{}, ctx.Err()
			}
			lastStatus = 1
			select {
			case <-ctx.Done():
				return Session{}, ctx.Err()
			case <-time.After(qrPollInterval):
			}
			continue
		}
		switch status {
		case qrStatusOK:
			if redirect == "" {
				return Session{}, errNoRedirect
			}
			q.report("扫码已确认，正在换取登录会话…")
			target, err := exchangeTeleaiTicket(ctx, q.base.Client, q.base.UserAgent, q.base.Referer, q.base.ClientID, q.base.RedirectURI, redirect)
			if err != nil {
				return Session{}, err
			}
			if err := followLoginRedirect(ctx, q.base.Client, q.base.UserAgent, target); err != nil {
				return Session{}, err
			}
			session, err := sessionFromJar(q.base.Client, q.now())
			if err != nil {
				return Session{}, err
			}
			q.report("扫码登录成功。")
			return session, nil
		case qrStatusExpired, qrStatusPageExpired:
			q.report("二维码已过期，正在刷新…")
			if err := q.Refresh(ctx); err != nil {
				return Session{}, err
			}
			deadline = q.now().Add(qrLoginTimeout)
			lastStatus = 1
		case qrStatusScanned:
			if lastStatus != qrStatusScanned {
				q.report("二维码已扫描，请在手机上确认登录。")
			}
		case qrStatusWaiting:
			if lastStatus != qrStatusWaiting {
				q.report("等待扫码…")
			}
		case qrStatusPassword:
			return Session{}, &LoginError{Code: strconv.Itoa(qrStatusPassword), Message: "该账号扫码后被要求密码验证，请改用账号密码登录"}
		}
		lastStatus = status
		select {
		case <-ctx.Done():
			return Session{}, ctx.Err()
		case <-time.After(qrPollInterval):
		}
	}
}

// pollOnce 发一次 qrcodeLoginState.do，返回状态码与成功时的回跳地址。
func (q *QRLogin) pollOnce(ctx context.Context) (int, string, error) {
	now := q.now()
	form := url.Values{
		"appId":       {q.AppID},
		"encryuuid":   {q.EncryUUID},
		"date":        {qrDate(now, int(now.UnixNano()%qrDateRandomBase))},
		"uuid":        {q.UUID},
		"returnUrl":   {url.QueryEscape(q.base.ReturnURL)},
		"clientType":  {"1"},
		"timeStamp":   {strconv.FormatInt(now.UnixMilli(), 10)},
		"cb_SaveName": {"0"},
		"isOauth2":    {strconv.FormatBool(q.base.IsOAuth2)},
		"state":       {q.base.State},
		"paramId":     {q.base.ParamID},
	}
	raw, err := post189Form(ctx, q.base.Client, s189QRStatePath, q.base.Referer, q.base.UserAgent, q.base.LT, q.base.ReqID, form)
	if err != nil {
		return 0, "", err
	}
	return decodeQRStatus(raw)
}

// qrEnvelope 是 getUUID.do 与 qrcodeLoginState.do 的扁平响应。
type qrEnvelope struct {
	Result      int    `json:"result"`
	Status      int    `json:"status"`
	UUID        string `json:"uuid"`
	EncryUUID   string `json:"encryuuid"`
	EncodeUUID  string `json:"encodeuuid"`
	RedirectURL string `json:"redirectUrl"`
	Msg         string `json:"msg"`
}

// decodeQRUUID 解析 getUUID.do：result 必须为 0，且三个 uuid 都要有。
func decodeQRUUID(raw []byte) (qrEnvelope, error) {
	var payload qrEnvelope
	if err := json.Unmarshal(raw, &payload); err != nil {
		return qrEnvelope{}, fmt.Errorf("teleagent: 解析二维码响应: %w", err)
	}
	if payload.Result != smsLoginResultOK || strings.TrimSpace(payload.UUID) == "" || strings.TrimSpace(payload.EncodeUUID) == "" || strings.TrimSpace(payload.EncryUUID) == "" {
		return qrEnvelope{}, errS189SignInvalid
	}
	return payload, nil
}

// decodeQRStatus 解析 qrcodeLoginState.do：失败时把 result 当状态码返回。
func decodeQRStatus(raw []byte) (int, string, error) {
	var payload qrEnvelope
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0, "", fmt.Errorf("teleagent: 解析扫码状态: %w", err)
	}
	switch {
	case payload.Status != 0:
		return payload.Status, payload.RedirectURL, nil
	case strings.TrimSpace(payload.RedirectURL) != "":
		return payload.Status, payload.RedirectURL, nil
	case payload.Result != 0:
		return payload.Result, "", nil
	}
	return payload.Status, payload.RedirectURL, nil
}

// qrImageURL 拼二维码图片地址；encodeuuid 不能再做 URL 转义。
func qrImageURL(base *s189LoginBase, encodeUUID string) string {
	requestID := ""
	if base != nil {
		requestID = base.ReqID
	}
	return s189Origin + s189QRImagePath + "?uuid=" + encodeUUID + "&REQID=" + url.QueryEscape(requestID)
}

// qrDate 复刻前端 date 字段：yyyy-MM-ddHH:mm:ss + 0~23 的随机尾数。
func qrDate(now time.Time, suffix int) string {
	if suffix < 0 {
		suffix = 0
	}
	return now.Format("2006-01-02") + now.Format("15:04:05") + strconv.Itoa(suffix)
}

// looksLikeImage 判断字节是否是可显示的 JPEG/PNG。
func looksLikeImage(raw []byte) bool {
	if len(raw) < 4 {
		return false
	}
	if raw[0] == 0xFF && raw[1] == 0xD8 {
		return true
	}
	return raw[0] == 0x89 && raw[1] == 0x50 && raw[2] == 0x4E && raw[3] == 0x47
}
