package teleagent

import (
	"context"
	"regexp"
	"testing"
	"time"
)

func TestQRDateFormatMatchesFrontend(t *testing.T) {
	fixed := time.Date(2026, 11, 18, 8, 3, 15, 0, time.Local)
	pattern := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\d{2}:\d{2}:\d{2}\d{1,2}$`)
	for suffix := 0; suffix < 24; suffix++ {
		value := qrDate(fixed, suffix)
		if !pattern.MatchString(value) {
			t.Fatalf("qrDate(%d) = %q，不符合前端格式", suffix, value)
		}
	}
	if got, want := qrDate(fixed, 17), "2026-11-1808:03:1517"; got != want {
		t.Fatalf("qrDate = %q，期望 %q", got, want)
	}
	if got, want := qrDate(fixed, -1), "2026-11-1808:03:150"; got != want {
		t.Fatalf("qrDate 负数尾数 = %q，期望 %q", got, want)
	}
}

func TestDecodeQRUUID(t *testing.T) {
	raw := []byte(`{"result":0,"uuid":"abc123","encryuuid":"ENC","encodeuuid":"ENC%3D"}`)
	payload, err := decodeQRUUID(raw)
	if err != nil {
		t.Fatalf("decodeQRUUID: %v", err)
	}
	if payload.UUID != "abc123" || payload.EncryUUID != "ENC" || payload.EncodeUUID != "ENC%3D" {
		t.Fatalf("解析结果不对: %+v", payload)
	}
	if _, err := decodeQRUUID([]byte(`{"result":0,"uuid":"abc123","encodeuuid":"x"}`)); err == nil {
		t.Fatal("缺少 encryuuid 应当报错")
	}
	if _, err := decodeQRUUID([]byte(`{"result":-1,"msg":"busy"}`)); err == nil {
		t.Fatal("result 非 0 应当报错")
	}
}

func TestDecodeQRStatus(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		status   int
		redirect string
	}{
		{"未扫码", `{"status":-106}`, -106, ""},
		{"已扫码", `{"status":-11002}`, -11002, ""},
		{"过期", `{"status":-11001}`, -11001, ""},
		{"页面过期", `{"result":-20099,"msg":"页面已过期"}`, -20099, ""},
		{"成功", `{"status":0,"redirectUrl":"https://www.teleai.com.cn/transfer?appId=a&paras=b&sign=c"}`, 0, "https://www.teleai.com.cn/transfer?appId=a&paras=b&sign=c"},
	}
	for _, testCase := range cases {
		status, redirect, err := decodeQRStatus([]byte(testCase.raw))
		if err != nil {
			t.Fatalf("%s: %v", testCase.name, err)
		}
		if status != testCase.status || redirect != testCase.redirect {
			t.Fatalf("%s: status=%d redirect=%q，期望 %d / %q", testCase.name, status, redirect, testCase.status, testCase.redirect)
		}
	}
	if _, _, err := decodeQRStatus([]byte("not-json")); err == nil {
		t.Fatal("非法 JSON 应当报错")
	}
}

func TestQRImageURL(t *testing.T) {
	base := &s189LoginBase{ReqID: "req123"}
	got := qrImageURL(base, "ENC%253D")
	want := "https://open.e.189.cn/api/logbox/oauth2/image.do?uuid=ENC%253D&REQID=req123"
	if got != want {
		t.Fatalf("qrImageURL = %q，期望 %q", got, want)
	}
}

func TestLooksLikeImage(t *testing.T) {
	if !looksLikeImage([]byte{0xFF, 0xD8, 0xFF, 0xE0}) {
		t.Fatal("JPEG 头应当被识别")
	}
	if !looksLikeImage([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D}) {
		t.Fatal("PNG 头应当被识别")
	}
	if looksLikeImage([]byte(`{"result":-1}`)) {
		t.Fatal("JSON 不应被当成图片")
	}
	if looksLikeImage(nil) {
		t.Fatal("空字节不应被当成图片")
	}
}

func TestQRLoginWaitRequiresState(t *testing.T) {
	if _, err := (&QRLogin{}).Wait(context.Background()); err == nil {
		t.Fatal("缺 base 的 QRLogin 应当报错")
	}
	if err := (&QRLogin{}).Refresh(context.Background()); err == nil {
		t.Fatal("缺 base 的 QRLogin 刷新应当报错")
	}
}
