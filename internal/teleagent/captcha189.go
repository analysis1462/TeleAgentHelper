package teleagent

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// 天翼账号（open.e.189.cn）的图形验证码是网易易盾的滑块组件。它对外只有两个
// JSONP 接口：get.do 取图、check.do 交答案。两个接口的请求都被包了一层：
// 明文 JSON 用一次性 AES-128-CBC 密钥加密成 pb，密钥本身用 RSA-1024 加密成 cp。
// 这里按网页端 captch.min.js 的逻辑把两个接口完整复刻出来，这样当 189 要求
// 图形验证码时，程序可以自己算缺口并交卷，不需要人工拖滑块。
const (
	s189CaptchaGetPath   = "/gw/captcha/get.do"
	s189CaptchaCheckPath = "/gw/captcha/check.do"
	s189CaptchaVersion   = "1.0.1"
	s189CaptchaWidth     = "310"
	s189CaptchaTypeCode  = 1
	// s189CaptchaFingerprint 是网页端上报的设备指纹。抓包实测 captch.min.js 上报的
	// 是数字 218789835（不是摘要字符串），两次独立会话取值一致，按固定值使用。
	s189CaptchaFingerprint = 218789835
	// s189CaptchaResultOK 是 189 验证码接口表示成功的 result 值。
	s189CaptchaResultOK = "0"
	// s189CaptchaTries 是自动过一次图形验证码的最大尝试次数：缺口算错就换一张。
	s189CaptchaTries = 3
	// 交卷时上报的拖动轨迹：点数量与单步耗时范围都按真人拖动取值。
	s189CaptchaDragSteps = 40
	s189CaptchaDragMinMS = 12
	s189CaptchaDragMaxMS = 36
	// s189CaptchaRSAPublicKey 是 189 图形验证码接口的 RSA-1024 公钥（SPKI base64），
	// 摘自登录页 captch.min.js。它只用来加密一次性 AES 密钥。
	s189CaptchaRSAPublicKey = "MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDky91Sokyr2UI/K87VMiZp/Pmiggg4fFKgclUZoPCoO+FvdeU/wSvv59Z6fEZi4Uvtzzv5UqCMfFRykokoiGSq8B3X1kr24RbtsWif/+pxfRDCA8tXw3V2DIZ/a03tg8BBgQLpdWuwTmM1448WFIs5O9pyFgjKDFoo5cWvs88HBQIDAQAB"
)

// 189 验证码接口的错误码。
const (
	s189CaptchaCodeRejected = "50002"
	s189CaptchaCodeServer   = "59999"
	s189CaptchaCodeExpired  = "50001"
	s189CaptchaCodeBadParam = "58888"
)

// s189GetPayload 是 get.do 的明文结构，字段顺序与网页端一致。
type s189GetPayload struct {
	AppID       string `json:"appId"`
	CaptchaType int    `json:"captchaType"`
	Referer     string `json:"referer"`
	Time        int64  `json:"time"`
	Finger      int64  `json:"finger"`
	Width       string `json:"width"`
}

// s189CheckPoint 是交卷时报的拼图块位移。
type s189CheckPoint struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// s189CheckRate 是交卷时报的单步位移与耗时。
type s189CheckRate struct {
	PointDiff int `json:"pointDiff"`
	TimeDiff  int `json:"timeDiff"`
}

// s189CheckPayload 是 check.do 的明文结构。
type s189CheckPayload struct {
	Token       string           `json:"token"`
	CaptchaType int              `json:"captchaType"`
	Points      []s189CheckPoint `json:"points"`
	Rates       []s189CheckRate  `json:"rates"`
	DragTime    int64            `json:"dragTime"`
	Time        int64            `json:"time"`
	Finger      int64            `json:"finger"`
}

// s189CaptchaChallenge 是一次取图的结果。
type s189CaptchaChallenge struct {
	Token      string
	Background image.Image
	Piece      image.Image
}

// s189CaptchaSolution 是一次成功交卷的结果，登录接口要用 token 与 validate。
type s189CaptchaSolution struct {
	Token    string
	Validate string
}

// s189Envelope 包装一次请求的 pb/cp 与它们共用的一次性密钥。
type s189Envelope struct {
	PB    string
	CP    string
	Key   []byte
	ReqID string
}

// marshalS189Plain 复刻网页端 JSON.stringify 的字节形态：不转义 < > &，也不带换行。
// 服务端会把解密后的明文与网页端上报的形态比对，Go 默认的 \u0026 转义会让长度和内容
// 都对不上，直接被判参数错误（58888）。
func marshalS189Plain(payload any) ([]byte, error) {
	buffer := &bytes.Buffer{}
	encoder := json.NewEncoder(buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}

// newS189Envelope 按网页端做法生成 pb/cp：AES 密钥与 IV 是同一个随机 16 字节串，
// pb 是密文的大写 hex，cp 是 RSA-1024(PKCS1v15，明文为密钥的 base64 字符串) 的 hex。
func newS189Envelope(plaintext []byte) (s189Envelope, error) {
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return s189Envelope{}, fmt.Errorf("teleagent: 生成验证码密钥: %w", err)
	}
	cipherText, err := aesCBCEncrypt(key, plaintext)
	if err != nil {
		return s189Envelope{}, err
	}
	publicKey, err := s189CaptchaPublicKey()
	if err != nil {
		return s189Envelope{}, err
	}
	encryptedKey, err := rsa.EncryptPKCS1v15(rand.Reader, publicKey, []byte(base64.StdEncoding.EncodeToString(key)))
	if err != nil {
		return s189Envelope{}, fmt.Errorf("teleagent: 加密验证码密钥: %w", err)
	}
	requestID, err := newS189RequestID()
	if err != nil {
		return s189Envelope{}, fmt.Errorf("teleagent: 生成验证码请求号: %w", err)
	}
	return s189Envelope{
		PB:    strings.ToUpper(hex.EncodeToString(cipherText)),
		CP:    hex.EncodeToString(encryptedKey),
		Key:   key,
		ReqID: requestID,
	}, nil
}

// newS189RequestID 生成网页端形态的请求号：32 位小写字母加数字。网页端用的是
// base36 随机串，服务端对字符集有校验，纯 hex 会被判参数错误。
func newS189RequestID() (string, error) {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	out := make([]byte, len(raw))
	for index, value := range raw {
		out[index] = alphabet[int(value)%len(alphabet)]
	}
	return string(out), nil
}

// s189CaptchaPublicKey 解码内置的 RSA 公钥。
func s189CaptchaPublicKey() (*rsa.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s189CaptchaRSAPublicKey))
	if err != nil {
		return nil, errS189CaptchaPublicKey
	}
	parsed, err := x509.ParsePKIXPublicKey(raw)
	if err != nil {
		return nil, errS189CaptchaPublicKey
	}
	publicKey, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, errS189CaptchaPublicKey
	}
	return publicKey, nil
}

// decryptS189Payload 用请求自带的密钥解开服务端回传的密文（base64 文本）。
func decryptS189Payload(key []byte, encoded string) ([]byte, error) {
	cipherText, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("teleagent: 解码验证码回包: %w", err)
	}
	return aesCBCDecrypt(key, cipherText)
}

// aesCBCEncrypt 按网页端的 AES/CBC/PKCS7 加密：key 与 IV 是同 16 字节字符串。
func aesCBCEncrypt(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("teleagent: 初始化验证码密钥: %w", err)
	}
	if len(key) != block.BlockSize() {
		return nil, errS189CaptchaDecrypt
	}
	padded := pkcs7Pad(plaintext, block.BlockSize())
	cipherText := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, key).CryptBlocks(cipherText, padded)
	return cipherText, nil
}

// aesCBCDecrypt 与 aesCBCEncrypt 对称。
func aesCBCDecrypt(key, cipherText []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("teleagent: 初始化解密密钥: %w", err)
	}
	if len(key) != block.BlockSize() || len(cipherText) == 0 || len(cipherText)%block.BlockSize() != 0 {
		return nil, errS189CaptchaDecrypt
	}
	plain := make([]byte, len(cipherText))
	cipher.NewCBCDecrypter(block, key).CryptBlocks(plain, cipherText)
	return pkcs7Unpad(plain, block.BlockSize())
}

// pkcs7Unpad 去掉 CBC 填充。
func pkcs7Unpad(data []byte, size int) ([]byte, error) {
	if len(data) == 0 || len(data)%size != 0 {
		return nil, errS189CaptchaDecrypt
	}
	padding := int(data[len(data)-1])
	if padding <= 0 || padding > size || padding > len(data) {
		return nil, errS189CaptchaDecrypt
	}
	for index := len(data) - padding; index < len(data); index++ {
		if int(data[index]) != padding {
			return nil, errS189CaptchaDecrypt
		}
	}
	return data[:len(data)-padding], nil
}

// s189ParseJSONP 把 callback_1712({...}) 拆成 JSON 对象。
func s189ParseJSONP(raw []byte) (map[string]any, error) {
	text := strings.TrimSpace(string(raw))
	start := strings.IndexByte(text, '(')
	end := strings.LastIndexByte(text, ')')
	if start < 0 || end <= start {
		return nil, fmt.Errorf("teleagent: 验证码接口返回了非 JSONP 内容: %.80s", text)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text[start+1:end]), &payload); err != nil {
		return nil, fmt.Errorf("teleagent: 解析验证码回包: %w", err)
	}
	return payload, nil
}

// s189ResultFields 取出 result 与 msg 字段。
func s189ResultFields(payload map[string]any) (string, string) {
	result := ""
	if value, ok := payload["result"]; ok {
		result = strings.TrimSpace(fmt.Sprint(value))
	}
	message := ""
	if value, ok := payload["msg"]; ok && value != nil {
		message = strings.TrimSpace(fmt.Sprint(value))
	}
	return result, message
}

// dataURL 去掉 data:image/...;base64, 前缀。
func dataURLPayload(encoded string) string {
	if index := strings.IndexByte(encoded, ','); index >= 0 {
		return encoded[index+1:]
	}
	return encoded
}

// decodeS189Image 解码 data URL 形式的验证码图片。
func decodeS189Image(encoded string) (image.Image, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(dataURLPayload(encoded)))
	if err != nil {
		return nil, fmt.Errorf("teleagent: 解码验证码图片: %w", err)
	}
	if decoded, jpegErr := jpeg.Decode(bytes.NewReader(raw)); jpegErr == nil {
		return decoded, nil
	}
	decoded, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("teleagent: 解析验证码图片: %w", err)
	}
	return decoded, nil
}

// s189CaptchaCall 按网页端形态发一次 JSONP 请求。
func (s *SMSLogin) s189CaptchaCall(ctx context.Context, path string, envelope s189Envelope) (map[string]any, error) {
	query := urlValuesOrdered(
		"pb", envelope.PB,
		"cp", envelope.CP,
		"appId", s.AppID,
		"version", s189CaptchaVersion,
		"reqId", envelope.ReqID,
		"callback", "callback_"+strconv.FormatInt(time.Now().UnixMilli(), 10),
	)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s189Origin+path+"?"+query, nil)
	if err != nil {
		return nil, fmt.Errorf("teleagent: 构造验证码请求: %w", err)
	}
	request.Header.Set("User-Agent", s.userAgent)
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Referer", s.referer)
	response, err := s.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("teleagent: 请求验证码接口 %s: %w", path, err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("teleagent: 读取验证码响应 %s: %w", path, err)
	}
	return s189ParseJSONP(raw)
}

// urlValuesOrdered 按给定顺序拼查询串：pb/cp 长度固定，服务端对顺序敏感度不明，
// 这里与网页端保持完全一致的顺序。
func urlValuesOrdered(pairs ...string) string {
	builder := strings.Builder{}
	for index := 0; index+1 < len(pairs); index += 2 {
		if index > 0 {
			builder.WriteByte('&')
		}
		builder.WriteString(pairs[index])
		builder.WriteByte('=')
		// appId 之类的值里可能出现 + / =，必须按查询串规则转义。
		builder.WriteString(url.QueryEscape(pairs[index+1]))
	}
	return builder.String()
}

// fetch189Captcha 取一张新图。
func (s *SMSLogin) fetch189Captcha(ctx context.Context) (s189CaptchaChallenge, error) {
	payload := s189GetPayload{
		AppID:       s.AppID,
		CaptchaType: s189CaptchaTypeCode,
		Referer:     s.referer,
		Time:        time.Now().UnixMilli(),
		Finger:      s189CaptchaFingerprint,
		Width:       s189CaptchaWidth,
	}
	plain, err := marshalS189Plain(payload)
	if err != nil {
		return s189CaptchaChallenge{}, fmt.Errorf("teleagent: 构造取图请求: %w", err)
	}
	envelope, err := newS189Envelope(plain)
	if err != nil {
		return s189CaptchaChallenge{}, err
	}
	response, err := s.s189CaptchaCall(ctx, s189CaptchaGetPath, envelope)
	if err != nil {
		return s189CaptchaChallenge{}, err
	}
	result, message := s189ResultFields(response)
	if result != s189CaptchaResultOK {
		return s189CaptchaChallenge{}, fmt.Errorf("teleagent: 获取 189 图形验证码失败（%s %s）", result, message)
	}
	data, ok := response["data"].(map[string]any)
	if !ok {
		return s189CaptchaChallenge{}, errS189CaptchaPayload
	}
	token := strings.TrimSpace(fmt.Sprint(data["token"]))
	backgroundText := strings.TrimSpace(fmt.Sprint(data["bg"]))
	pieceText := strings.TrimSpace(fmt.Sprint(data["front"]))
	if token == "" || backgroundText == "" || pieceText == "" {
		return s189CaptchaChallenge{}, errS189CaptchaPayload
	}
	background, err := decodeS189Image(backgroundText)
	if err != nil {
		return s189CaptchaChallenge{}, err
	}
	piece, err := decodeS189Image(pieceText)
	if err != nil {
		return s189CaptchaChallenge{}, err
	}
	return s189CaptchaChallenge{Token: token, Background: background, Piece: piece}, nil
}

// solve189Offset 用灰度归一化互相关找出拼图块在底图中的横坐标。
//
// 189 的拼图块带描边与阴影，与底图对应区域并非逐像素一致，直接比像素差
// 区分度很差；换成去均值的互相关系数后最佳位置通常在 0.9 以上。
func solve189Offset(background, piece image.Image) (int, error) {
	backgroundBounds := background.Bounds()
	pieceBounds := piece.Bounds()
	maxX := backgroundBounds.Dx() - pieceBounds.Dx()
	if maxX < 0 || pieceBounds.Dy() <= 0 {
		return 0, errCaptchaImageMismatch
	}
	// 先把拼图块的不透明像素与灰度值取出来，循环里只做加权求和。
	type sample struct {
		x, y int
		lum  float64
	}
	samples := make([]sample, 0, pieceBounds.Dx()*pieceBounds.Dy())
	for y := 0; y < pieceBounds.Dy(); y += 2 {
		for x := 0; x < pieceBounds.Dx(); x++ {
			red, green, blue, alpha := piece.At(pieceBounds.Min.X+x, pieceBounds.Min.Y+y).RGBA()
			if alpha < 0x8000 {
				continue
			}
			samples = append(samples, sample{
				x:   x,
				y:   y,
				lum: 0.299*float64(red>>8) + 0.587*float64(green>>8) + 0.114*float64(blue>>8),
			})
		}
	}
	if len(samples) == 0 {
		return 0, errCaptchaImageMismatch
	}
	pieceMean := 0.0
	for _, item := range samples {
		pieceMean += item.lum
	}
	pieceMean /= float64(len(samples))
	pieceDeviation := 0.0
	for _, item := range samples {
		delta := item.lum - pieceMean
		pieceDeviation += delta * delta
	}
	if pieceDeviation == 0 {
		return 0, errCaptchaImageMismatch
	}

	best, bestScore := -1, math.Inf(-1)
	for offset := 0; offset <= maxX; offset++ {
		windowMean := 0.0
		window := make([]float64, len(samples))
		for index, item := range samples {
			red, green, blue, _ := background.At(backgroundBounds.Min.X+offset+item.x, backgroundBounds.Min.Y+item.y).RGBA()
			window[index] = 0.299*float64(red>>8) + 0.587*float64(green>>8) + 0.114*float64(blue>>8)
			windowMean += window[index]
		}
		windowMean /= float64(len(samples))
		covariance, windowDeviation := 0.0, 0.0
		for index, item := range samples {
			delta := window[index] - windowMean
			covariance += (item.lum - pieceMean) * delta
			windowDeviation += delta * delta
		}
		if windowDeviation == 0 {
			continue
		}
		score := covariance / math.Sqrt(pieceDeviation*windowDeviation)
		if score > bestScore {
			best, bestScore = offset, score
		}
	}
	if best < 0 {
		return 0, errCaptchaImageMismatch
	}
	return best, nil
}

// buildS189Trace 按真人拖动造一条轨迹：位移总和等于缺口坐标，耗时在合理区间。
func buildS189Trace(offset int) ([]s189CheckRate, int64) {
	rates := make([]s189CheckRate, 0, s189CaptchaDragSteps+1)
	rates = append(rates, s189CheckRate{PointDiff: 0, TimeDiff: 0})
	if offset <= 0 {
		return rates, int64(s189CaptchaDragMinMS)
	}
	// 按累计位移分配每一步的增量，保证所有 pointDiff 之和正好等于缺口坐标。
	totalMS := int64(0)
	previous := 0
	for step := 1; step <= s189CaptchaDragSteps; step++ {
		current := offset * step / s189CaptchaDragSteps
		diff := current - previous
		previous = current
		gap := s189CaptchaDragMinMS
		if step%3 == 0 {
			gap = s189CaptchaDragMaxMS
		}
		totalMS += int64(gap)
		rates = append(rates, s189CheckRate{PointDiff: diff, TimeDiff: gap})
	}
	return rates, totalMS
}

// check189Captcha 交卷，成功时返回服务端回传的 validate 票据。
func (s *SMSLogin) check189Captcha(ctx context.Context, challenge s189CaptchaChallenge, offset int) (string, error) {
	rates, dragTime := buildS189Trace(offset)
	payload := s189CheckPayload{
		Token:       challenge.Token,
		CaptchaType: s189CaptchaTypeCode,
		Points:      []s189CheckPoint{{X: offset, Y: 0}},
		Rates:       rates,
		DragTime:    dragTime,
		Time:        time.Now().UnixMilli(),
		Finger:      s189CaptchaFingerprint,
	}
	plain, err := marshalS189Plain(payload)
	if err != nil {
		return "", fmt.Errorf("teleagent: 构造交卷请求: %w", err)
	}
	envelope, err := newS189Envelope(plain)
	if err != nil {
		return "", err
	}
	response, err := s.s189CaptchaCall(ctx, s189CaptchaCheckPath, envelope)
	if err != nil {
		return "", err
	}
	result, message := s189ResultFields(response)
	if result != s189CaptchaResultOK {
		return "", &s189CaptchaError{Code: result, Message: message}
	}
	// 成功时 data 是用本次请求密钥加密的 base64 文本，解开后取 validate。
	encoded, ok := response["data"].(string)
	if !ok {
		return "", errS189CaptchaPayload
	}
	decrypted, err := decryptS189Payload(envelope.Key, encoded)
	if err != nil {
		return "", err
	}
	var decoded map[string]any
	if err := json.Unmarshal(decrypted, &decoded); err != nil {
		return "", fmt.Errorf("teleagent: 解析交卷结果: %w", err)
	}
	validate := strings.TrimSpace(fmt.Sprint(decoded["validate"]))
	if validate == "" {
		return "", errS189CaptchaPayload
	}
	return validate, nil
}

// s189CaptchaError 保留 189 验证码接口的错误码，便于上层给出可操作的提示。
type s189CaptchaError struct {
	Code    string
	Message string
}

func (e *s189CaptchaError) Error() string {
	if e == nil {
		return ""
	}
	return "teleagent: 189 图形验证码校验失败（" + e.Code + " " + firstNonEmpty(e.Message, "未知原因") + "）"
}

// solve189Captcha 取图、算缺口、交卷，最多试 s189CaptchaTries 次。
func (s *SMSLogin) solve189Captcha(ctx context.Context) (s189CaptchaSolution, error) {
	var lastErr error
	for attempt := 1; attempt <= s189CaptchaTries; attempt++ {
		challenge, err := s.fetch189Captcha(ctx)
		if err != nil {
			return s189CaptchaSolution{}, err
		}
		offset, err := solve189Offset(challenge.Background, challenge.Piece)
		if err != nil {
			lastErr = err
			continue
		}
		s.report(fmt.Sprintf("189 图形验证码：第 %d 次取图，算出缺口 x=%d，正在交卷…", attempt, offset))
		validate, err := s.check189Captcha(ctx, challenge, offset)
		if err == nil {
			return s189CaptchaSolution{Token: challenge.Token, Validate: validate}, nil
		}
		lastErr = err
		if captchaErr, ok := err.(*s189CaptchaError); ok {
			switch captchaErr.Code {
			case s189CaptchaCodeExpired, s189CaptchaCodeRejected:
				continue
			case s189CaptchaCodeServer, s189CaptchaCodeBadParam:
				// 服务端异常或参数被判定非法时换图也无意义，直接返回带诊断的提示。
				return s189CaptchaSolution{}, s189CaptchaFailure(captchaErr)
			}
		}
	}
	if lastErr == nil {
		lastErr = errCaptchaRejected
	}
	if captchaErr, ok := lastErr.(*s189CaptchaError); ok {
		return s189CaptchaSolution{}, s189CaptchaFailure(captchaErr)
	}
	return s189CaptchaSolution{}, fmt.Errorf("teleagent: 189 图形验证码自动处理失败（%v），请改用账号密码登录，或稍后重试", lastErr)
}

// s189CaptchaFailure 把服务端错误码翻译成用户能照着做的提示。
func s189CaptchaFailure(captchaErr *s189CaptchaError) error {
	switch captchaErr.Code {
	case s189CaptchaCodeServer:
		return fmt.Errorf("teleagent: 189 图形验证码服务返回系统异常（59999），这是天翼账号侧的问题；该手机号现在必须过图形验证码才能发短信，建议改用账号密码登录，或过几小时再试")
	case s189CaptchaCodeBadParam:
		return fmt.Errorf("teleagent: 189 图形验证码接口判定参数非法（58888），登录页的加密参数可能已升级；建议改用账号密码登录")
	default:
		return captchaErr
	}
}
