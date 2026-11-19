package teleagent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	// smsBrowserTimeout 是浏览器兜底换票的总时长。
	smsBrowserTimeout = 75 * time.Second
	// smsBrowserPoll 是轮询浏览器 cookie 的间隔。
	smsBrowserPoll = 500 * time.Millisecond
)

// completeSMSTicketInBrowser 让本机浏览器自己完成票据换会话。
//
// 189 短信登录成功后只会给一个 /transfer 单页应用地址，真正的换票（
// openLoginCallbackV2 -> userAccountList -> chooseUserAccountLogin -> 回调）
// 由那个页面在浏览器里执行。纯 HTTP 复刻一旦被服务端策略挡住，就走这条兜底：
// 用全新的临时 profile 打开回跳地址，把此前 HTTP 阶段已经拿到的 platform cookie
// 预先种进浏览器，让页面接着走完换票，再把 x-token 取出来。
func completeSMSTicketInBrowser(ctx context.Context, target string, seed []Cookie) (Session, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return Session{}, errNoRedirect
	}
	browserPath, err := findBrowser()
	if err != nil {
		return Session{}, err
	}
	profileDir, err := os.MkdirTemp("", "teleagent-sms-browser-")
	if err != nil {
		return Session{}, fmt.Errorf("teleagent: 创建临时浏览器目录: %w", err)
	}
	defer func() { _ = os.RemoveAll(profileDir) }()

	port, err := freePort()
	if err != nil {
		return Session{}, err
	}
	runCtx, cancel := context.WithTimeout(ctx, smsBrowserTimeout)
	defer cancel()

	arguments := append([]string{"--headless=new"}, browserArguments(port, profileDir, "about:blank")...)
	command := exec.Command(browserPath, arguments...)
	if err := command.Start(); err != nil {
		return Session{}, fmt.Errorf("teleagent: 启动浏览器兜底换票: %w", err)
	}
	defer func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		_, _ = command.Process.Wait()
	}()

	devtools := fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := waitForDevTools(runCtx, devtools); err != nil {
		return Session{}, err
	}
	page, err := waitForPageTarget(runCtx, devtools, "")
	if err != nil {
		return Session{}, err
	}
	conn, err := dialCDP(runCtx, page.WebSocketDebuggerURL)
	if err != nil {
		return Session{}, err
	}
	defer conn.close()
	if _, err := conn.call(runCtx, "Network.enable", nil); err != nil {
		return Session{}, err
	}
	if _, err := conn.call(runCtx, "Runtime.enable", nil); err != nil {
		return Session{}, err
	}
	if len(seed) > 0 {
		if err := seedBrowserCookies(runCtx, conn, seed); err != nil {
			return Session{}, err
		}
	}
	if _, err := conn.call(runCtx, "Page.enable", nil); err != nil {
		return Session{}, err
	}
	if _, err := conn.call(runCtx, "Page.navigate", map[string]any{"url": target}); err != nil {
		return Session{}, err
	}

	for {
		select {
		case <-runCtx.Done():
			return Session{}, errors.New("teleagent: 浏览器兜底换票超时，登录页可能已过期")
		case <-time.After(smsBrowserPoll):
		}
		session, err := captureSession(runCtx, conn)
		if err != nil {
			continue
		}
		if strings.TrimSpace(session.Token) != "" {
			return session, nil
		}
	}
}

// seedBrowserCookies 把 HTTP 阶段已经获得的平台 cookie 种进浏览器。
func seedBrowserCookies(ctx context.Context, conn *cdpConn, seed []Cookie) error {
	rows := make([]map[string]any, 0, len(seed))
	for _, cookie := range seed {
		domain := strings.TrimSpace(cookie.Domain)
		if domain == "" {
			domain = defaultCookieDomain
		}
		row := map[string]any{
			"name":     cookie.Name,
			"value":    cookie.Value,
			"domain":   domain,
			"path":     firstNonEmpty(cookie.Path, "/"),
			"secure":   cookie.Secure,
			"httpOnly": cookie.HTTPOnly,
		}
		if cookie.Expires > 0 {
			row["expires"] = cookie.Expires
		}
		rows = append(rows, row)
	}
	_, err := conn.call(ctx, "Network.setCookies", map[string]any{"cookies": rows})
	return err
}

// defaultCookieDomain 是 TeleAgent 平台 cookie 的域。
const defaultCookieDomain = ".teleai.com.cn"

// platformCookieURLs 是读取 cookie 时使用的探测地址：Go 的 cookie jar 按
// 域与路径筛选，单看源站根路径会漏掉挂在业务路径上的 cookie。
var platformCookieURLs = []string{
	DefaultOrigin,
	DefaultOrigin + "/",
	DefaultOrigin + agentCallbackPath,
	DefaultOrigin + "/#/chat",
	LoginOrigin,
	LoginOrigin + "/",
	LoginOrigin + "/transfer",
}

// collectPlatformCookies 汇总 cookie 容器里所有平台侧 cookie（按名去重）。
func collectPlatformCookies(client *http.Client) []Cookie {
	if client == nil || client.Jar == nil {
		return nil
	}
	seen := make(map[string]struct{}, 8)
	var cookies []Cookie
	for _, raw := range platformCookieURLs {
		parsed, err := url.Parse(raw)
		if err != nil {
			continue
		}
		for _, cookie := range client.Jar.Cookies(parsed) {
			key := cookie.Name + "|" + cookie.Domain
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			value := cookie.Value
			if decoded, decodeErr := url.QueryUnescape(value); decodeErr == nil {
				value = decoded
			}
			cookies = append(cookies, Cookie{
				Name:     cookie.Name,
				Value:    value,
				Domain:   cookie.Domain,
				Path:     cookie.Path,
				Expires:  cookie.Expires.Unix(),
				Secure:   cookie.Secure,
				HTTPOnly: cookie.HttpOnly,
			})
		}
	}
	return cookies
}
