package teleagent

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	crand "crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"image"
	"image/color"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/uvwt/CtyunHelper/internal/storage"
)

func TestEncryptCaptchaPointMatchesCryptoJS(t *testing.T) {
	key := "1234567890abcdef"
	cases := []struct {
		plain string
		want  string
	}{
		{"{\"x\":109,\"y\":5}", "cCBEg5ke97sOUMQKi0hWXQ=="},
		{"tok---{\"x\":109,\"y\":5}", "tD4RDpHUxkHFE7YK/lMGRdAMDAV3TxfldgT83DX1Jlk="},
		{"{\"x\":10,\"y\":5}", "500XicIFdX7SrfxGLb3I3g=="},
	}
	for _, tc := range cases {
		got, err := encryptCaptchaPoint(key, tc.plain)
		if err != nil {
			t.Fatalf("encryptCaptchaPoint(%q): %v", tc.plain, err)
		}
		if got != tc.want {
			t.Fatalf("encryptCaptchaPoint(%q) = %q, 期望 %q", tc.plain, got, tc.want)
		}
	}
	if _, err := encryptCaptchaPoint("short", "x"); err == nil {
		t.Fatal("期望密钥长度非法时报错")
	}
}

func TestEncodeJSONStringMatchesBrowser(t *testing.T) {
	cases := map[string]string{
		"a\"b":          "\"a\\\"b\"",
		"a\\b":          "\"a\\\\b\"",
		"a<b>&c":        "\"a<b>&c\"",
		"first\nsecond": "\"first\\nsecond\"",
		"tab\there":     "\"tab\\there\"",
		"中文密码":          "\"中文密码\"",
	}
	for input, want := range cases {
		got, err := encodeJSONString(input)
		if err != nil {
			t.Fatalf("encodeJSONString(%q): %v", input, err)
		}
		if string(got) != want {
			t.Fatalf("encodeJSONString(%q) = %s, 期望 %s", input, got, want)
		}
	}
}

// TestSolveOffsetFindsJigsaw 用合成图片验证逐列匹配能把拼图块定位回原始横坐标。
func TestSolveOffsetFindsJigsaw(t *testing.T) {
	const (
		width  = 310
		height = 155
		pieceW = 47
		pieceY = 30
		wantX  = 123
	)
	source := rand.New(rand.NewSource(20261118))
	background := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			background.Set(x, y, color.RGBA{
				R: uint8(source.Intn(256)),
				G: uint8(source.Intn(256)),
				B: uint8(source.Intn(256)),
				A: 0xff,
			})
		}
	}
	piece := image.NewRGBA(image.Rect(0, 0, pieceW, height))
	for y := 0; y < height; y++ {
		for x := 0; x < pieceW; x++ {
			if y < pieceY || y >= pieceY+pieceW {
				piece.Set(x, y, color.RGBA{})
				continue
			}
			piece.Set(x, y, background.At(wantX+x, y))
		}
	}
	got, err := solveOffset(background, piece)
	if err != nil {
		t.Fatalf("solveOffset: %v", err)
	}
	if got != wantX {
		t.Fatalf("solveOffset = %d, 期望 %d", got, wantX)
	}
}

func TestAESGCMSealBindsAdditionalData(t *testing.T) {
	key := bytes.Repeat([]byte{0x2b}, 16)
	iv := bytes.Repeat([]byte{0x07}, 12)
	plain := []byte("\"password\"")
	ciphertext, tag, err := aesGCMSeal(key, iv, plain, []byte("loginName=a&keyId=b"))
	if err != nil {
		t.Fatalf("aesGCMSeal: %v", err)
	}
	if len(ciphertext) != len(plain) || len(tag) != 16 {
		t.Fatalf("密文/标签长度异常: %d/%d", len(ciphertext), len(tag))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, len(iv))
	if err != nil {
		t.Fatalf("NewGCMWithNonceSize: %v", err)
	}
	sealed := append(append([]byte{}, ciphertext...), tag...)
	decrypted, err := gcm.Open(nil, iv, sealed, []byte("loginName=a&keyId=b"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(decrypted, plain) {
		t.Fatalf("解密结果 %q, 期望 %q", decrypted, plain)
	}
	if _, err := gcm.Open(nil, iv, sealed, []byte("loginName=a&keyId=c")); err == nil {
		t.Fatal("期望 AAD 不一致时校验失败")
	}
}

func TestCredentialsRoundTrip(t *testing.T) {
	paths := storage.Paths{
		ConfigDir: t.TempDir(),
		DataDir:   filepath.Join(t.TempDir(), "data"),
	}
	if _, err := LoadCredentials(paths); err != ErrNoCredentials {
		t.Fatalf("空目录应返回 ErrNoCredentials，得到 %v", err)
	}
	if err := SaveCredentials(paths, Credentials{Username: "demo_user", Password: "p@ss"}); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	loaded, err := LoadCredentials(paths)
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if loaded.Username != "demo_user" || loaded.Password != "p@ss" {
		t.Fatalf("凭据不一致: %+v", loaded)
	}
	if err := DeleteCredentials(paths); err != nil {
		t.Fatalf("DeleteCredentials: %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths.DataDir, credentialFileName)); !os.IsNotExist(err) {
		t.Fatalf("凭据文件应被删除，得到 %v", err)
	}
}
func TestLooksLikeJWT(t *testing.T) {
	// 头段 eyJhbGciOiJIUzI1NiJ9 解出来是 {"alg":"HS256"}。
	if !looksLikeJWT("eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2ln") {
		t.Fatal("合法 JWT 应被识别")
	}
	for _, value := range []string{"", "abc", "a.b", "a.b.c", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0"} {
		if looksLikeJWT(value) {
			t.Fatalf("%q 不应被识别为 JWT", value)
		}
	}
}

func TestTokenFromCookiesPrefersKnownName(t *testing.T) {
	cookies := []Cookie{
		{Name: "opencowork-auth", Value: "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2ln"},
		{Name: "x-token", Value: "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIyIn0.c2ln"},
	}
	if got := tokenFromCookies(cookies); got != "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIyIn0.c2ln" {
		t.Fatalf("应优先取 x-token，得到 %q", got)
	}
	fallback := []Cookie{{Name: "renamed", Value: "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIzIn0.c2ln"}}
	if got := tokenFromCookies(fallback); got == "" {
		t.Fatal("已知名字缺失时应回退到 JWT 形态的 cookie")
	}
	if got := tokenFromCookies([]Cookie{{Name: "sid", Value: "plain-value"}}); got != "" {
		t.Fatalf("非 JWT 值不应被当成令牌，得到 %q", got)
	}
}

func TestCollectPlatformCookiesDedupes(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	origin, err := url.Parse(DefaultOrigin)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	jar.SetCookies(origin, []*http.Cookie{
		{Name: "x-token", Value: "tok", Domain: ".teleai.com.cn", Path: "/"},
		{Name: "operationToken", Value: "op", Domain: ".teleai.com.cn", Path: "/"},
	})
	client := &http.Client{Jar: jar}
	cookies := collectPlatformCookies(client)
	if len(cookies) != 2 {
		t.Fatalf("应汇总到 2 个 cookie，得到 %d：%+v", len(cookies), cookies)
	}
	if got := tokenFromCookies(cookies); got != "tok" {
		t.Fatalf("令牌应为 tok，得到 %q", got)
	}
}
func TestBuildLoginPayloadUsesRawPassword(t *testing.T) {
	key, err := rsa.GenerateKey(crand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成 RSA 密钥: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("导出公钥: %v", err)
	}
	keyData := rsaKeyData{KeyID: "key-id", PublicKey: base64.StdEncoding.EncodeToString(der)}
	const password = "Abc123!xy"
	payload, err := buildLoginPayload("user", password, "client", "https://example.com/", keyData, "ticket", time.Now())
	if err != nil {
		t.Fatalf("buildLoginPayload: %v", err)
	}
	ciphertext, _ := payload["ciphertext"].(string)
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		t.Fatalf("ciphertext 不是 base64: %v", err)
	}
	if len(raw) != len(password) {
		t.Fatalf("密文长度 %d，期望等于密码长度 %d（明文不应带 JSON 引号）", len(raw), len(password))
	}
}
