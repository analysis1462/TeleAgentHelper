package teleagent

import (
	"bytes"
	"context"
	"crypto/aes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"strings"
)

// 滑块验证码（blockPuzzle）走的是上游 upms 的公共接口，与登录页同源。
const (
	captchaGetPath   = "/upms/public/captcha/get"
	captchaCheckPath = "/upms/public/captcha/check"
	captchaType      = "blockPuzzle"
	captchaPointY    = 5
	// captchaImageWidth 是上游服务端计算缺口坐标时使用的图片宽度基准。
	captchaImageWidth = 310
	captchaMaxRetries = 5
)

// captchaSolver 负责取图、求解缺口、换取一次性票据。
//
// 上游只校验缺口横坐标：pointJson 里恒为 {x, y=5}，没有轨迹数组，
// 因此这里直接做像素级模板匹配，不需要伪造拖动轨迹。
type captchaSolver struct {
	http  *http.Client
	api   string
	agent string
}

func newCaptchaSolver(client *http.Client, api, agent string) *captchaSolver {
	return &captchaSolver{http: client, api: strings.TrimRight(api, "/"), agent: strings.TrimRight(agent, "/")}
}

type captchaChallenge struct {
	Token      string
	SecretKey  string
	Background image.Image
	Piece      image.Image
}

type captchaEnvelope struct {
	RepCode string          `json:"repCode"`
	RepMsg  string          `json:"repMsg"`
	RepData json.RawMessage `json:"repData"`
	Success bool            `json:"success"`
}

type captchaData struct {
	OriginalImageBase64 string `json:"originalImageBase64"`
	JigsawImageBase64   string `json:"jigsawImageBase64"`
	Token               string `json:"token"`
	SecretKey           string `json:"secretKey"`
	Result              bool   `json:"result"`
}

func (s *captchaSolver) post(ctx context.Context, path string, body any) (captchaEnvelope, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return captchaEnvelope{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.api+path, bytes.NewReader(payload))
	if err != nil {
		return captchaEnvelope{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/plain, */*")
	request.Header.Set("Origin", "https://www.teleai.com.cn")
	request.Header.Set("Referer", "https://www.teleai.com.cn/master-auth")
	response, err := s.http.Do(request)
	if err != nil {
		return captchaEnvelope{}, fmt.Errorf("teleagent: 请求验证码接口 %s: %w", path, err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return captchaEnvelope{}, fmt.Errorf("teleagent: 读取验证码响应 %s: %w", path, err)
	}
	var env captchaEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return captchaEnvelope{}, fmt.Errorf("teleagent: 解析验证码响应 %s: %w", path, err)
	}
	return env, nil
}

func decodeBase64PNG(encoded string) (image.Image, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("teleagent: 解码验证码图片: %w", err)
	}
	decoded, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("teleagent: 解析验证码图片: %w", err)
	}
	return decoded, nil
}

func (s *captchaSolver) fetch(ctx context.Context) (captchaChallenge, error) {
	env, err := s.post(ctx, captchaGetPath, map[string]any{"captchaType": captchaType})
	if err != nil {
		return captchaChallenge{}, err
	}
	if env.RepCode != "0000" {
		return captchaChallenge{}, fmt.Errorf("teleagent: 获取滑块验证码失败（%s）", firstNonEmpty(env.RepMsg, env.RepCode))
	}
	var data captchaData
	if err := json.Unmarshal(env.RepData, &data); err != nil {
		return captchaChallenge{}, fmt.Errorf("teleagent: 解析滑块验证码: %w", err)
	}
	background, err := decodeBase64PNG(data.OriginalImageBase64)
	if err != nil {
		return captchaChallenge{}, err
	}
	piece, err := decodeBase64PNG(data.JigsawImageBase64)
	if err != nil {
		return captchaChallenge{}, err
	}
	return captchaChallenge{Token: data.Token, SecretKey: data.SecretKey, Background: background, Piece: piece}, nil
}

// solveOffset 用逐像素平均差找出拼图块在底图中的横坐标。
func solveOffset(background, piece image.Image) (int, error) {
	backgroundBounds := background.Bounds()
	pieceBounds := piece.Bounds()
	maxX := backgroundBounds.Dx() - pieceBounds.Dx()
	if maxX < 0 {
		return 0, errCaptchaImageMismatch
	}
	best, bestScore := -1, 1<<62
	for offset := 0; offset <= maxX; offset++ {
		sum, count := 0, 0
		for y := 0; y < pieceBounds.Dy(); y += 2 {
			for x := 0; x < pieceBounds.Dx(); x++ {
				pr, pg, pb, pa := piece.At(pieceBounds.Min.X+x, pieceBounds.Min.Y+y).RGBA()
				if pa < 0x8000 {
					continue
				}
				br, bg, bb, _ := background.At(backgroundBounds.Min.X+offset+x, backgroundBounds.Min.Y+y).RGBA()
				sum += absDiff(int(pr>>8), int(br>>8)) + absDiff(int(pg>>8), int(bg>>8)) + absDiff(int(pb>>8), int(bb>>8))
				count++
			}
		}
		if count == 0 {
			continue
		}
		if score := sum / count; score < bestScore {
			best, bestScore = offset, score
		}
	}
	if best < 0 {
		return 0, errCaptchaImageMismatch
	}
	return best, nil
}

func absDiff(a, b int) int {
	if a > b {
		return a - b
	}
	return b - a
}

func (s *captchaSolver) verify(ctx context.Context, challenge captchaChallenge, offset int) error {
	point := fmt.Sprintf("{\"x\":%d,\"y\":%d}", offset, captchaPointY)
	if err := s.checkPoint(ctx, challenge, point); err != nil {
		return err
	}
	return nil
}

func (s *captchaSolver) checkPoint(ctx context.Context, challenge captchaChallenge, point string) error {
	pointJSON, err := encryptCaptchaPoint(challenge.SecretKey, point)
	if err != nil {
		return err
	}
	env, err := s.post(ctx, captchaCheckPath, map[string]any{
		"captchaType": captchaType,
		"pointJson":   pointJSON,
		"token":       challenge.Token,
	})
	if err != nil {
		return err
	}
	if env.RepCode != "0000" {
		return fmt.Errorf("teleagent: 滑块校验失败（%s）", firstNonEmpty(env.RepMsg, env.RepCode))
	}
	var data captchaData
	if err := json.Unmarshal(env.RepData, &data); err != nil {
		return fmt.Errorf("teleagent: 解析滑块校验结果: %w", err)
	}
	if !data.Result {
		return errCaptchaRejected
	}
	return nil
}

// ticket 生成登录接口需要的 captchaVerification 票据。
func (s *captchaSolver) ticket(ctx context.Context) (string, error) {
	var lastErr error
	for attempt := 0; attempt < captchaMaxRetries; attempt++ {
		challenge, err := s.fetch(ctx)
		if err != nil {
			return "", err
		}
		offset, err := solveOffset(challenge.Background, challenge.Piece)
		if err != nil {
			return "", err
		}
		point := fmt.Sprintf("{\"x\":%d,\"y\":%d}", offset, captchaPointY)
		if err := s.checkPoint(ctx, challenge, point); err != nil {
			lastErr = err
			continue
		}
		return encryptCaptchaPoint(challenge.SecretKey, challenge.Token+"---"+point)
	}
	if lastErr == nil {
		lastErr = errCaptchaRejected
	}
	return "", lastErr
}

// encryptCaptchaPoint 复刻前端的 AES-ECB + PKCS7 + base64（CryptoJS 默认填充）。
func encryptCaptchaPoint(secretKey, plaintext string) (string, error) {
	key := []byte(secretKey)
	if len(key) != 16 && len(key) != 24 && len(key) != 32 {
		return "", errCaptchaSecretKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("teleagent: 初始化验证码加密: %w", err)
	}
	padded := pkcs7Pad([]byte(plaintext), block.BlockSize())
	encrypted := make([]byte, len(padded))
	for offset := 0; offset < len(padded); offset += block.BlockSize() {
		block.Encrypt(encrypted[offset:offset+block.BlockSize()], padded[offset:offset+block.BlockSize()])
	}
	return base64.StdEncoding.EncodeToString(encrypted), nil
}

func pkcs7Pad(data []byte, size int) []byte {
	padding := size - len(data)%size
	result := make([]byte, len(data)+padding)
	copy(result, data)
	for index := len(data); index < len(result); index++ {
		result[index] = byte(padding)
	}
	return result
}
