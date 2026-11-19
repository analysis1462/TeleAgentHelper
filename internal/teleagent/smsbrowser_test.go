package teleagent

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// syntheticCaptcha 生成确定性的纹理底图与被抠出来的拼图条带。
func syntheticCaptcha(t *testing.T, width, height, pieceWidth, answer, pieceTop, pieceHeight int) (image.Image, image.Image) {
	t.Helper()
	background := image.NewRGBA(image.Rect(0, 0, width, height))
	seed := uint32(20261118)
	next := func() uint8 {
		seed = seed*1664525 + 1013904223
		return uint8(seed >> 24)
	}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			background.Set(x, y, color.RGBA{R: next(), G: next(), B: next(), A: 0xFF})
		}
	}
	piece := image.NewRGBA(image.Rect(0, 0, pieceWidth, height))
	for y := pieceTop; y < pieceTop+pieceHeight; y++ {
		for x := 4; x < pieceWidth-2; x++ {
			red, green, blue, _ := background.At(answer+x, y).RGBA()
			piece.Set(x, y, color.RGBA{R: uint8(red >> 8), G: uint8(green >> 8), B: uint8(blue >> 8), A: 0xFF})
		}
	}
	return background, piece
}

func TestSolveSMSOffsetFindsAnswer(t *testing.T) {
	const answer = 137
	background, piece := syntheticCaptcha(t, 310, 155, 47, answer, 92, 43)
	offset, peak, err := solveSMSOffset(background, piece)
	if err != nil {
		t.Fatalf("solve: %v", err)
	}
	if offset != answer {
		t.Fatalf("偏移 = %d，期望 %d", offset, answer)
	}
	if peak < 0.9 {
		t.Fatalf("置信度 %.3f 过低", peak)
	}
}

func TestSolveSMSOffsetRejectsEmptyMask(t *testing.T) {
	background := image.NewRGBA(image.Rect(0, 0, 310, 155))
	piece := image.NewRGBA(image.Rect(0, 0, 47, 155))
	if _, _, err := solveSMSOffset(background, piece); err == nil {
		t.Fatal("空拼图块应当报错")
	}
}

func TestSolveSMSOffsetRejectsSizeMismatch(t *testing.T) {
	background := image.NewRGBA(image.Rect(0, 0, 40, 155))
	_, piece := syntheticCaptcha(t, 310, 155, 47, 10, 92, 43)
	if _, _, err := solveSMSOffset(background, piece); err == nil {
		t.Fatal("底图比拼图还窄时应当报错")
	}
}

func TestDecodeBrowserImageJPEGAndPNG(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			source.Set(x, y, color.RGBA{R: uint8(x * 30), G: uint8(y * 30), B: 90, A: 0xFF})
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, source); err != nil {
		t.Fatalf("编码 PNG: %v", err)
	}
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(buffer.Bytes())
	decoded, err := decodeBrowserImage(dataURL)
	if err != nil {
		t.Fatalf("解码: %v", err)
	}
	if decoded.Bounds().Dx() != 8 || decoded.Bounds().Dy() != 8 {
		t.Fatalf("尺寸 = %v", decoded.Bounds())
	}
	if _, err := decodeBrowserImage("data:image/png;base64,not-base64!!"); err == nil {
		t.Fatal("非法 base64 应当报错")
	}
}

func TestS189CheckPassed(t *testing.T) {
	passed := [2]string{0: `callback({"result":"0","data":"c29tZQ=="})`, 1: `callback({"result":"0"})`}
	for _, body := range passed {
		if !s189CheckPassed(body) {
			t.Fatalf("应当判定通过：%s", body)
		}
	}
	failed := [2]string{0: `callback({"result":"50002","data":null})`, 1: `not json`}
	for _, body := range failed {
		if s189CheckPassed(body) {
			t.Fatalf("不应判定通过：%s", body)
		}
	}
}

func TestShortText(t *testing.T) {
	if got := shortText("  0123456789  ", 4); got != "0123…" {
		t.Fatalf("shortText = %q", got)
	}
	if got := shortText("abc", 0); got != "abc" {
		t.Fatalf("shortText = %q", got)
	}
}
func TestLooksLikeTenantJWT(t *testing.T) {
	const jwt = "eyJhbGciOiJIUzI1NiJ9.eyJ0ZW5hbnQiOiJ4In0.c2ln"
	if !looksLikeTenantJWT("tenantA:" + jwt) {
		t.Fatal("tenant:jwt 形态应当被识别")
	}
	if looksLikeTenantJWT(jwt) {
		t.Fatal("裸 JWT 不应走 tenant 判定")
	}
	if looksLikeTenantJWT("tenantA:") {
		t.Fatal("只有前缀不算")
	}
}
