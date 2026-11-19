package teleagent

import (
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"strings"
	"testing"
)

func TestS189ParseJSONP(t *testing.T) {
	raw := []byte(`callback_1712345678901({"result":"0","msg":"","data":{"token":"tok-1","bg":"data:image/jpg;base64,AA"}})`)
	payload, err := s189ParseJSONP(raw)
	if err != nil {
		t.Fatalf("parse jsonp: %v", err)
	}
	result, message := s189ResultFields(payload)
	if result != "0" || message != "" {
		t.Fatalf("result=%q msg=%q", result, message)
	}
	data, ok := payload["data"].(map[string]any)
	if !ok {
		t.Fatal("data 不是对象")
	}
	if data["token"] != "tok-1" {
		t.Fatalf("token=%v", data["token"])
	}
	if _, err := s189ParseJSONP([]byte("not jsonp")); err == nil {
		t.Fatal("非 JSONP 内容应当报错")
	}
}

func TestNewS189EnvelopeRoundTrip(t *testing.T) {
	plain := []byte(`{"token":"abc","captchaType":1}`)
	envelope, err := newS189Envelope(plain)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if len(envelope.Key) != 16 {
		t.Fatalf("key 长度=%d", len(envelope.Key))
	}
	if len(envelope.PB) == 0 || strings.ToUpper(envelope.PB) != envelope.PB {
		t.Fatalf("pb 应当是大写 hex: %q", envelope.PB)
	}
	// RSA-1024 密文 128 字节，hex 后 256 字符。
	if len(envelope.CP) != smsRSASize {
		t.Fatalf("cp 长度=%d", len(envelope.CP))
	}
	if len(envelope.ReqID) != 32 {
		t.Fatalf("reqId 长度=%d", len(envelope.ReqID))
	}
	cipherText, err := hex.DecodeString(envelope.PB)
	if err != nil {
		t.Fatalf("pb 不是 hex: %v", err)
	}
	decrypted, err := decryptS189Payload(envelope.Key, base64String(cipherText))
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(decrypted) != string(plain) {
		t.Fatalf("往返不一致: %q", decrypted)
	}
}

func TestBuildS189TraceSumsToOffset(t *testing.T) {
	for _, offset := range []int{0, 37, 123, 263} {
		rates, dragTime := buildS189Trace(offset)
		total := 0
		for index, rate := range rates {
			total += rate.PointDiff
			if index == 0 {
				if rate.PointDiff != 0 || rate.TimeDiff != 0 {
					t.Fatalf("首点应当是 0/0: %+v", rate)
				}
				continue
			}
			if rate.TimeDiff < s189CaptchaDragMinMS || rate.TimeDiff > s189CaptchaDragMaxMS {
				t.Fatalf("单步耗时越界: %d", rate.TimeDiff)
			}
		}
		if offset > 0 && total != offset {
			t.Fatalf("offset=%d 轨迹总和=%d", offset, total)
		}
		if offset > 0 && dragTime <= 0 {
			t.Fatalf("offset=%d dragTime=%d", offset, dragTime)
		}
	}
}

func TestSolve189OffsetSynthetic(t *testing.T) {
	const width, height, pieceWidth, target = 310, 155, 47, 120
	background := image.NewGray(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			background.SetGray(x, y, color.Gray{Y: uint8((x*7 + y*13) % 200)})
		}
	}
	piece := image.NewRGBA(image.Rect(0, 0, pieceWidth, height))
	for y := 0; y < height; y++ {
		for x := 0; x < pieceWidth; x++ {
			value := background.GrayAt(target+x, y).Y
			piece.Set(x, y, color.RGBA{R: value, G: value, B: value, A: 0xFF})
		}
	}
	offset, err := solve189Offset(background, piece)
	if err != nil {
		t.Fatalf("solve: %v", err)
	}
	if offset != target {
		t.Fatalf("offset=%d 期望 %d", offset, target)
	}
}

func TestS189CaptchaFailureMessage(t *testing.T) {
	message := s189CaptchaFailure(&s189CaptchaError{Code: s189CaptchaCodeServer, Message: "系统异常"}).Error()
	if !strings.Contains(message, "59999") {
		t.Fatalf("提示里应当带错误码: %s", message)
	}
	for _, banned := range []string{"无法", "不能", "抱歉"} {
		if strings.Contains(message, banned) {
			t.Fatalf("提示里出现了要避开的措辞 %q: %s", banned, message)
		}
	}
}

// base64String 是测试里用的小工具，避免测试文件再引一个 import 别名。
func base64String(raw []byte) string {
	const table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var builder strings.Builder
	for index := 0; index < len(raw); index += 3 {
		remaining := len(raw) - index
		first := raw[index]
		second, third := byte(0), byte(0)
		if remaining > 1 {
			second = raw[index+1]
		}
		if remaining > 2 {
			third = raw[index+2]
		}
		builder.WriteByte(table[first>>2])
		builder.WriteByte(table[((first&0x03)<<4)|(second>>4)])
		if remaining > 1 {
			builder.WriteByte(table[((second&0x0F)<<2)|(third>>6)])
		} else {
			builder.WriteByte('=')
		}
		if remaining > 2 {
			builder.WriteByte(table[third&0x3F])
		} else {
			builder.WriteByte('=')
		}
	}
	return builder.String()
}

var _ = fmt.Sprintf
