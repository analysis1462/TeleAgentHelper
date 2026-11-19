package teleagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/uvwt/CtyunHelper/internal/logging"
)

// DefaultLoginTimeout 是等待用户完成登录的最长时间。
const DefaultLoginTimeout = 5 * time.Minute

// LoginOptions 控制"打开浏览器手动登录 → 捕获会话"的流程。
type LoginOptions struct {
	Origin      string
	BrowserPath string
	ProfileDir  string
	Timeout     time.Duration
	Logger      *logging.Logger
	OnProgress  func(string)
}

type cdpTarget struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

type cdpCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"`
	Secure   bool    `json:"secure"`
	HTTPOnly bool    `json:"httpOnly"`
}

// LoginWithBrowser 打开系统浏览器让用户手动登录，并从该会话里取出凭据。
// 密码登录必须过图形验证码，所以登录动作由人完成，之后完全自动化。
func LoginWithBrowser(ctx context.Context, options LoginOptions) (Session, error) {
	origin := strings.TrimRight(strings.TrimSpace(options.Origin), "/")
	if origin == "" {
		origin = DefaultOrigin
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = DefaultLoginTimeout
	}
	progress := options.OnProgress
	if progress == nil {
		progress = func(string) {}
	}
	browserPath := options.BrowserPath
	if browserPath == "" {
		path, err := findBrowser()
		if err != nil {
			return Session{}, err
		}
		browserPath = path
	}
	profileDir := options.ProfileDir
	if profileDir == "" {
		cacheDir, err := os.UserCacheDir()
		if err != nil {
			return Session{}, err
		}
		profileDir = filepath.Join(cacheDir, "CtyunHelper", "teleagent-profile")
	}
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		return Session{}, err
	}
	port, err := freePort()
	if err != nil {
		return Session{}, err
	}

	deadline := time.Now().Add(timeout)
	loginCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	command := exec.Command(browserPath, browserArguments(port, profileDir, origin+"/teleagent")...)
	if err := command.Start(); err != nil {
		return Session{}, fmt.Errorf("teleagent: 启动浏览器: %w", err)
	}
	defer func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		_, _ = command.Process.Wait()
	}()
	progress("已打开浏览器，请在窗口里完成登录（滑块/短信验证需要手动过）")

	devtools := fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := waitForDevTools(loginCtx, devtools); err != nil {
		return Session{}, err
	}
	target, err := waitForPageTarget(loginCtx, devtools, "teleai")
	if err != nil {
		return Session{}, err
	}
	conn, err := dialCDP(loginCtx, target.WebSocketDebuggerURL)
	if err != nil {
		return Session{}, err
	}
	defer conn.close()
	if _, err := conn.call(loginCtx, "Network.enable", nil); err != nil {
		return Session{}, err
	}
	if _, err := conn.call(loginCtx, "Runtime.enable", nil); err != nil {
		return Session{}, err
	}
	progress("等待登录完成……")

	for {
		select {
		case <-loginCtx.Done():
			return Session{}, errors.New("teleagent: 等待登录超时，请重试")
		case <-time.After(time.Second):
		}
		session, err := captureSession(loginCtx, conn)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return Session{}, errors.New("teleagent: 等待登录超时，请重试")
			}
			continue
		}
		if session.Token == "" {
			continue
		}
		client := NewClient(ClientOptions{Origin: options.Origin, Session: session})
		info, infoErr := client.UserInfo(loginCtx)
		if infoErr != nil {
			if errors.Is(infoErr, ErrSessionExpired) {
				continue
			}
			continue
		}
		session.AccountID = firstNonEmpty(info.AccountID, session.AccountID)
		session.AccountName = firstNonEmpty(info.AccountName, session.AccountName)
		session.CapturedAt = time.Now()
		progress("登录成功：" + session.AccountName)
		if options.Logger != nil {
			options.Logger.Info("teleagent", "已捕获登录会话",
				logging.String("account", session.AccountName))
		}
		return session, nil
	}
}

func browserArguments(port int, profileDir, url string) []string {
	return []string{
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--user-data-dir=" + profileDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-features=msEdgeSidebarV2",
		"--disable-background-networking",
		"--disable-popup-blocking",
		"--window-size=1280,900",
		url,
	}
}

func findBrowser() (string, error) {
	candidates := []string{
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	for _, name := range []string{"msedge.exe", "chrome.exe"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", errors.New("teleagent: 未找到 Edge/Chrome，可用 --browser 指定可执行文件")
}

func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() {
		_ = listener.Close()
	}()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func waitForDevTools(ctx context.Context, base string) error {
	for {
		request, err := http.NewRequestWithContext(ctx, "GET", base+"/json/version", nil)
		if err != nil {
			return err
		}
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("teleagent: 浏览器调试端口未就绪")
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func waitForPageTarget(ctx context.Context, base, contains string) (cdpTarget, error) {
	for {
		request, err := http.NewRequestWithContext(ctx, "GET", base+"/json/list", nil)
		if err != nil {
			return cdpTarget{}, err
		}
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			raw, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			_ = response.Body.Close()
			if readErr == nil {
				var targets []cdpTarget
				if json.Unmarshal(raw, &targets) == nil {
					var fallback *cdpTarget
					for index := range targets {
						if targets[index].Type != "page" || targets[index].WebSocketDebuggerURL == "" {
							continue
						}
						if contains != "" && strings.Contains(targets[index].URL, contains) {
							return targets[index], nil
						}
						if fallback == nil {
							fallback = &targets[index]
						}
					}
					if fallback != nil {
						return *fallback, nil
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return cdpTarget{}, errors.New("teleagent: 浏览器没有可用的页面")
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func captureSession(ctx context.Context, conn *cdpConn) (Session, error) {
	var cookiesResult struct {
		Cookies []cdpCookie `json:"cookies"`
	}
	raw, err := conn.call(ctx, "Network.getAllCookies", nil)
	if err != nil {
		return Session{}, err
	}
	if err := json.Unmarshal(raw, &cookiesResult); err != nil {
		return Session{}, err
	}

	session := Session{AppVersion: DefaultAppVersion, CapturedAt: time.Now(), DeviceID: randomUUID()}
	for _, cookie := range cookiesResult.Cookies {
		if !strings.HasSuffix(strings.TrimPrefix(cookie.Domain, "."), "teleai.com.cn") {
			continue
		}
		expires := int64(0)
		if cookie.Expires > 0 {
			expires = int64(cookie.Expires)
		}
		session.Cookies = append(session.Cookies, Cookie{
			Name:     cookie.Name,
			Value:    cookie.Value,
			Domain:   cookie.Domain,
			Path:     cookie.Path,
			Expires:  expires,
			Secure:   cookie.Secure,
			HTTPOnly: cookie.HTTPOnly,
		})
	}

	for _, name := range []string{"x-token", "operationToken", "user_token"} {
		if value, ok := session.Cookie(name); ok && strings.Count(value, ".") >= 2 {
			session.Token = NewToken(value, "")
			break
		}
	}
	if session.Token == "" {
		if value, ok := session.Cookie("operationToken"); ok && value != "" {
			session.Token = value
		}
	}
	if session.Token == "" {
		if value := readLocalStorage(ctx, conn); value != "" {
			session.Token = NewToken(value, "")
		}
	}
	if session.Token == "" {
		if value := tokenFromCookies(session.Cookies); value != "" {
			session.Token = NewToken(value, "")
		}
	}
	session.Normalize()
	if len(session.Cookies) > 0 {
		if value, ok := session.Cookie("x-token"); ok {
			session.Token = firstNonEmpty(session.Token, value)
		}
	}
	return session, nil
}

func readLocalStorage(ctx context.Context, conn *cdpConn) string {
	const script = "(() => { try { const keys = ['opencowork-auth','sso_token','accessToken','console_token']; const out = {}; for (const key of keys) { out[key] = localStorage.getItem(key); } return JSON.stringify(out); } catch (error) { return ''; } })()"
	raw, err := conn.call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    script,
		"returnByValue": true,
	})
	if err != nil {
		return ""
	}
	var reply struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &reply) != nil {
		return ""
	}
	if strings.TrimSpace(reply.Result.Value) == "" {
		return ""
	}
	var stored map[string]*string
	if json.Unmarshal([]byte(reply.Result.Value), &stored) != nil {
		return ""
	}
	for _, key := range []string{"sso_token", "accessToken", "console_token"} {
		if value, ok := stored[key]; ok && value != nil && strings.TrimSpace(*value) != "" {
			return strings.TrimSpace(*value)
		}
	}
	if value, ok := stored["opencowork-auth"]; ok && value != nil {
		var auth struct {
			State struct {
				Token string `json:"token"`
			} `json:"state"`
			Token string `json:"token"`
		}
		if json.Unmarshal([]byte(*value), &auth) == nil {
			return firstNonEmpty(auth.Token, auth.State.Token)
		}
	}
	return ""
}
