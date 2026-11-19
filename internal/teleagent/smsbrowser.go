package teleagent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/uvwt/CtyunHelper/internal/logging"
)

const (
	// smsBrowserAuthURL 是天翼统一认证入口：必须从这里进入才能拿到 client_id 与流程 cookie。
	smsBrowserAuthURL = DefaultOrigin + agentCallbackPath
	// smsBrowserFrameMarker 用于在调试目标列表里认出 189 登录框（OOPIF 有独立 target）。
	smsBrowserFrameMarker = "open.e.189.cn/api/logbox"
	// smsBrowserDefaultTimeout 是「打开页面 + 发短信 + 过滑块」这一阶段的总预算。
	smsBrowserDefaultTimeout = 3 * time.Minute
	// smsBrowserDefaultAttempts 是最多尝试几次滑块。
	smsBrowserDefaultAttempts = 3
	// smsBrowserElementTimeout 是等待页面元素出现的时长。
	smsBrowserElementTimeout = 25 * time.Second
	// smsBrowserCheckTimeout 是等待 check.do 结果的时长。
	smsBrowserCheckTimeout = 8 * time.Second
	// smsBrowserStepDelay 是拖拽时每步的基准间隔（毫秒）。
	smsBrowserStepDelay = 14
	// smsBrowserMinPeak 是滑块模板匹配的最低置信度。
	smsBrowserMinPeak = 0.30
)

// SMSBrowserOptions 控制「浏览器发短信 + 自动过 189 滑块」的流程。
type SMSBrowserOptions struct {
	// Phone 是接收短信的手机号。
	Phone string
	// BrowserPath 可指定 Edge/Chrome 可执行文件，留空自动查找。
	BrowserPath string
	// Timeout 是本阶段总时长，默认 3 分钟。
	Timeout time.Duration
	// Attempts 是滑块最多尝试次数，默认 3 次。
	Attempts int
	// OnProgress 用于把进度回显给界面。
	OnProgress func(string)
	// Logger 可选，用于落日志。
	Logger *logging.Logger
}

// SMSBrowserLogin 是一次仍在进行的浏览器登录：窗口保持打开，短信发出后由调用方
// 提交验证码，再从同一个浏览器会话里把 cookie 取出来。
type SMSBrowserLogin struct {
	phone    string
	page     *cdpConn
	frame    *cdpConn
	command  *exec.Cmd
	profile  string
	checks   chan string
	progress func(string)
	logger   *logging.Logger
	mu       sync.Mutex
	closed   bool
}

// StartSMSBrowserLogin 打开发送短信所需的浏览器流程，并自动通过图形验证码。
//
// 189 的滑块只在「可见窗口」里才会绑定事件监听（headless 下 DOMDebugger 读到 0 个
// 监听，怎么拖都没反应），所以这里必须弹出真实窗口；窗口用临时 profile，
// 不会碰到用户自己的登录态。
func StartSMSBrowserLogin(ctx context.Context, options SMSBrowserOptions) (*SMSBrowserLogin, error) {
	phone := normalizePhone(options.Phone)
	if !validMainlandPhone(phone) {
		return nil, errPhoneRequired
	}
	browserPath := strings.TrimSpace(options.BrowserPath)
	if browserPath == "" {
		found, err := findBrowser()
		if err != nil {
			return nil, err
		}
		browserPath = found
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = smsBrowserDefaultTimeout
	}
	attempts := options.Attempts
	if attempts <= 0 {
		attempts = smsBrowserDefaultAttempts
	}
	progress := options.OnProgress
	if progress == nil {
		progress = func(string) {}
	}
	profileDir, err := os.MkdirTemp("", "teleagent-189-login-")
	if err != nil {
		return nil, fmt.Errorf("teleagent: 创建临时浏览器目录: %w", err)
	}
	port, err := freePort()
	if err != nil {
		_ = os.RemoveAll(profileDir)
		return nil, err
	}
	login := &SMSBrowserLogin{
		phone:    phone,
		profile:  profileDir,
		checks:   make(chan string, 8),
		progress: progress,
		logger:   options.Logger,
	}
	command := exec.Command(browserPath, smsBrowserArguments(port, profileDir, smsBrowserAuthURL)...)
	if err := command.Start(); err != nil {
		_ = os.RemoveAll(profileDir)
		return nil, fmt.Errorf("teleagent: 启动浏览器: %w", err)
	}
	login.command = command

	startCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	devtools := fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := waitForDevTools(startCtx, devtools); err != nil {
		login.Close()
		return nil, err
	}
	login.report("已打开浏览器，正在等待天翼账号登录页…")
	target, err := waitForPageTarget(startCtx, devtools, "master-auth")
	if err != nil {
		login.Close()
		return nil, err
	}
	pageConn, err := dialCDP(startCtx, target.WebSocketDebuggerURL)
	if err != nil {
		login.Close()
		return nil, err
	}
	login.page = pageConn
	for _, method := range []string{"Page.enable", "Runtime.enable", "Network.enable"} {
		if _, err := pageConn.call(startCtx, method, nil); err != nil {
			login.Close()
			return nil, err
		}
	}
	frameTarget, err := waitForFrameTarget(startCtx, devtools, smsBrowserFrameMarker)
	if err != nil {
		login.Close()
		return nil, err
	}
	frameConn, err := dialCDP(startCtx, frameTarget.WebSocketDebuggerURL)
	if err != nil {
		login.Close()
		return nil, err
	}
	login.frame = frameConn
	for _, method := range []string{"Runtime.enable", "Network.enable", "DOM.enable"} {
		if _, err := frameConn.call(startCtx, method, nil); err != nil {
			login.Close()
			return nil, err
		}
	}
	frameConn.subscribe(login.onFrameEvent)

	if err := login.waitForElement(startCtx, "dynamicUserName"); err != nil {
		login.Close()
		return nil, err
	}
	quoted, err := encodeJSONString(phone)
	if err != nil {
		login.Close()
		return nil, err
	}
	fill := fmt.Sprintf("(function(){var el=document.getElementById('dynamicUserName');if(!el)return 'missing';var setter=Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype,'value').set;setter.call(el,%s);el.dispatchEvent(new Event('input',{bubbles:true}));el.dispatchEvent(new Event('change',{bubbles:true}));return el.value;})()", string(quoted))
	value, err := evalString(startCtx, frameConn, fill)
	if err != nil {
		login.Close()
		return nil, err
	}
	if value != phone {
		login.Close()
		return nil, errBrowserElementMissing
	}
	if err := login.clickCodeButton(startCtx); err != nil {
		login.Close()
		return nil, err
	}
	login.report("已提交手机号，正在自动完成图形验证码…")
	if err := login.solveCaptcha(startCtx, attempts); err != nil {
		login.Close()
		return nil, err
	}
	login.report("图形验证码已通过，短信验证码已发送。")
	return login, nil
}

// Phone 返回本次登录使用的手机号。
func (b *SMSBrowserLogin) Phone() string {
	if b == nil {
		return ""
	}
	return b.phone
}

// report 把进度同时回显到界面与日志。
func (b *SMSBrowserLogin) report(message string) {
	if b == nil {
		return
	}
	if b.progress != nil {
		b.progress(message)
	}
	if b.logger != nil {
		b.logger.Info("teleagent", message)
	}
}

// SubmitCode 把短信验证码填进页面并点击「登录」。
func (b *SMSBrowserLogin) SubmitCode(ctx context.Context, code string) error {
	if b == nil || b.frame == nil {
		return errSMSLoginMissing
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return errSMSCodeRequired
	}
	quoted, err := encodeJSONString(code)
	if err != nil {
		return err
	}
	fill := fmt.Sprintf("(function(){var el=document.getElementById('dynamicPassword');if(!el)return 'missing';var setter=Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype,'value').set;setter.call(el,%s);el.dispatchEvent(new Event('input',{bubbles:true}));el.dispatchEvent(new Event('change',{bubbles:true}));return el.value;})()", string(quoted))
	value, err := evalString(ctx, b.frame, fill)
	if err != nil {
		return err
	}
	if value == "missing" {
		return errSMSLoginMissing
	}
	point, err := b.elementCenter(ctx, "j-dynamicLogin")
	if err != nil {
		return err
	}
	if err := b.click(ctx, point); err != nil {
		return err
	}
	b.report("已提交验证码，正在等待登录完成…")
	return nil
}

// WaitSession 轮询浏览器会话，直到拿到可用登录态。
func (b *SMSBrowserLogin) WaitSession(ctx context.Context) (Session, error) {
	if b == nil || b.page == nil {
		return Session{}, errSMSLoginMissing
	}
	for {
		if err := ctx.Err(); err != nil {
			return Session{}, fmt.Errorf("teleagent: 浏览器登录未完成: %w", err)
		}
		session, err := b.CaptureSession(ctx)
		if err == nil && strings.TrimSpace(session.Token) != "" {
			client := NewClient(ClientOptions{Session: session})
			info, infoErr := client.UserInfo(ctx)
			if infoErr == nil {
				session.AccountID = firstNonEmpty(info.AccountID, session.AccountID)
				session.AccountName = firstNonEmpty(info.AccountName, session.AccountName)
				session.CapturedAt = time.Now()
				session.Normalize()
				return session, nil
			}
			if !errors.Is(infoErr, ErrSessionExpired) {
				return session, nil
			}
		}
		if err := sleepContext(ctx, smsBrowserPoll); err != nil {
			return Session{}, fmt.Errorf("teleagent: 浏览器登录未完成: %w", err)
		}
	}
}

// CaptureSession 立刻从浏览器里取一次会话（取不到时 Token 为空）。
func (b *SMSBrowserLogin) CaptureSession(ctx context.Context) (Session, error) {
	if b == nil || b.page == nil {
		return Session{}, errSMSLoginMissing
	}
	session, err := captureSession(ctx, b.page)
	if err != nil {
		return Session{}, err
	}
	if strings.TrimSpace(session.Token) == "" {
		if value, ok := session.Cookie("user_token"); ok && strings.TrimSpace(value) != "" {
			session.Token = NewToken(value, "")
			session.Normalize()
		}
	}
	return session, nil
}

// Close 关掉浏览器并清理临时 profile。
func (b *SMSBrowserLogin) Close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	b.mu.Unlock()
	if b.frame != nil {
		b.frame.close()
	}
	if b.page != nil {
		b.page.close()
	}
	if b.command != nil && b.command.Process != nil {
		_ = b.command.Process.Kill()
		_, _ = b.command.Process.Wait()
	}
	if b.profile != "" {
		for attempt := 0; attempt < 5; attempt++ {
			if err := os.RemoveAll(b.profile); err == nil {
				break
			}
			time.Sleep(400 * time.Millisecond)
		}
	}
}

// onFrameEvent 收集 189 滑块交卷（check.do）的结果。
func (b *SMSBrowserLogin) onFrameEvent(event cdpEvent) {
	if event.Method != "Network.responseReceived" {
		return
	}
	var params struct {
		RequestID string `json:"requestId"`
		Response  struct {
			URL string `json:"url"`
		} `json:"response"`
	}
	if json.Unmarshal(event.Params, &params) != nil {
		return
	}
	if !strings.Contains(params.Response.URL, "check.do") {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := b.frame.call(ctx, "Network.getResponseBody", map[string]any{"requestId": params.RequestID})
	if err != nil {
		return
	}
	var body struct {
		Body string `json:"body"`
	}
	if json.Unmarshal(raw, &body) != nil || strings.TrimSpace(body.Body) == "" {
		return
	}
	select {
	case b.checks <- body.Body:
	default:
	}
}

// solveCaptcha 反复「取图 - 求解 - 拖动 - 等结果」，直到 189 返回 result=0。
func (b *SMSBrowserLogin) solveCaptcha(ctx context.Context, attempts int) error {
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		listeners := 0
		for wait := 0; wait < 20; wait++ {
			listeners = b.sliderListeners(ctx)
			if listeners > 0 {
				break
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			_ = b.clickCodeButton(ctx)
			if err := sleepContext(ctx, 700*time.Millisecond); err != nil {
				return err
			}
		}
		if listeners == 0 {
			lastErr = errBrowserElementMissing
			continue
		}
		challenge, err := b.captcha(ctx)
		if err != nil {
			lastErr = err
			if err := sleepContext(ctx, 800*time.Millisecond); err != nil {
				return err
			}
			continue
		}
		offset, peak, err := solveSMSOffset(challenge.Background, challenge.Piece)
		if err != nil {
			lastErr = err
			continue
		}
		if peak < smsBrowserMinPeak {
			lastErr = fmt.Errorf("%w：匹配置信度 %.2f 过低", errCaptchaImageMismatch, peak)
			continue
		}
		b.report(fmt.Sprintf("第 %d 次滑块：缺口 x=%d（置信度 %.2f），正在拖动…", attempt, offset, peak))
		b.drainChecks()
		if err := b.drag(ctx, challenge.CenterX, challenge.CenterY, offset); err != nil {
			lastErr = err
			continue
		}
		body, err := b.waitCheck(ctx)
		if err != nil {
			lastErr = err
			if err := sleepContext(ctx, 800*time.Millisecond); err != nil {
				return err
			}
			continue
		}
		if s189CheckPassed(body) {
			return nil
		}
		lastErr = fmt.Errorf("%w：服务端返回 %s", errSMSCaptchaSolve, shortText(body, 60))
		if err := sleepContext(ctx, 1200*time.Millisecond); err != nil {
			return err
		}
	}
	if lastErr == nil {
		lastErr = errSMSCaptchaSolve
	}
	return fmt.Errorf("%w：%v", errSMSCaptchaSolve, lastErr)
}

// sliderListeners 返回滑块元素上的事件监听数量；为 0 说明组件还没初始化。
func (b *SMSBrowserLogin) sliderListeners(ctx context.Context) int {
	document, err := b.frame.call(ctx, "DOM.getDocument", map[string]any{"depth": -1})
	if err != nil {
		return 0
	}
	var doc struct {
		Root struct {
			NodeID int64 `json:"nodeId"`
		} `json:"root"`
	}
	if json.Unmarshal(document, &doc) != nil {
		return 0
	}
	query, err := b.frame.call(ctx, "DOM.querySelectorAll", map[string]any{
		"nodeId":   doc.Root.NodeID,
		"selector": "#j__21cn_ecaptch_slider",
	})
	if err != nil {
		return 0
	}
	var found struct {
		NodeIDs []int64 `json:"nodeIds"`
	}
	if json.Unmarshal(query, &found) != nil || len(found.NodeIDs) == 0 {
		return 0
	}
	resolved, err := b.frame.call(ctx, "DOM.resolveNode", map[string]any{"nodeId": found.NodeIDs[0]})
	if err != nil {
		return 0
	}
	var remote struct {
		Object struct {
			ObjectID string `json:"objectId"`
		} `json:"object"`
	}
	if json.Unmarshal(resolved, &remote) != nil || remote.Object.ObjectID == "" {
		return 0
	}
	listeners, err := b.frame.call(ctx, "DOMDebugger.getEventListeners", map[string]any{"objectId": remote.Object.ObjectID})
	if err != nil {
		return 0
	}
	var result struct {
		Listeners []json.RawMessage `json:"listeners"`
	}
	if json.Unmarshal(listeners, &result) != nil {
		return 0
	}
	return len(result.Listeners)
}

// smsCaptcha 是一张待解的滑块图。
type smsCaptcha struct {
	Background image.Image
	Piece      image.Image
	CenterX    float64
	CenterY    float64
}

// captcha 从页面里取出底图、拼图块与滑块中心坐标。
func (b *SMSBrowserLogin) captcha(ctx context.Context) (smsCaptcha, error) {
	const script = "(function(){var bg=document.getElementById('j__21cn_ecaptch_bg');var piece=document.getElementById('j__21cn_ecaptch_jigsaw');var slider=document.getElementById('j__21cn_ecaptch_slider');if(!bg||!piece||!slider)return null;var rect=slider.getBoundingClientRect();return {background:bg.src,piece:piece.src,centerX:Math.round(rect.x+rect.width/2),centerY:Math.round(rect.y+rect.height/2)};})()"
	raw, err := evalValue(ctx, b.frame, script)
	if err != nil {
		return smsCaptcha{}, err
	}
	var geometry struct {
		Background string  `json:"background"`
		Piece      string  `json:"piece"`
		CenterX    float64 `json:"centerX"`
		CenterY    float64 `json:"centerY"`
	}
	if err := json.Unmarshal(raw, &geometry); err != nil {
		return smsCaptcha{}, errCaptchaImageMismatch
	}
	if strings.TrimSpace(geometry.Background) == "" || strings.TrimSpace(geometry.Piece) == "" {
		return smsCaptcha{}, errCaptchaImageMismatch
	}
	background, err := decodeBrowserImage(geometry.Background)
	if err != nil {
		return smsCaptcha{}, err
	}
	piece, err := decodeBrowserImage(geometry.Piece)
	if err != nil {
		return smsCaptcha{}, err
	}
	return smsCaptcha{
		Background: background,
		Piece:      piece,
		CenterX:    geometry.CenterX,
		CenterY:    geometry.CenterY,
	}, nil
}

// drag 用 CDP 的受信鼠标事件把滑块拖动 answer+1 像素。
//
// 页面提交的 x 是「总位移减去首步步长」，而首步固定为 1 像素，因此总位移必须多拖 1。
func (b *SMSBrowserLogin) drag(ctx context.Context, centerX, centerY float64, offset int) error {
	total := offset + 1
	if _, err := b.frame.call(ctx, "Input.dispatchMouseEvent", map[string]any{
		"type": "mousePressed", "x": centerX, "y": centerY, "button": "left", "buttons": 1, "clickCount": 1,
	}); err != nil {
		return err
	}
	if err := sleepContext(ctx, 140*time.Millisecond); err != nil {
		return err
	}
	move := func(x, y float64) error {
		_, err := b.frame.call(ctx, "Input.dispatchMouseEvent", map[string]any{
			"type": "mouseMoved", "x": x, "y": y, "button": "left", "buttons": 1,
		})
		return err
	}
	if err := move(centerX+1, centerY); err != nil {
		return err
	}
	if err := sleepContext(ctx, 70*time.Millisecond); err != nil {
		return err
	}
	steps := total / 6
	if steps < 8 {
		steps = 8
	}
	current := 1
	for step := 1; step <= steps; step++ {
		position := 1 + (total-1)*step/steps
		if position == current && step != steps {
			continue
		}
		jitter := float64(step%5 - 2)
		if err := move(centerX+float64(position), centerY+jitter); err != nil {
			return err
		}
		if err := sleepContext(ctx, time.Duration(smsBrowserStepDelay+(step*11)%26)*time.Millisecond); err != nil {
			return err
		}
		current = position
	}
	if current != total {
		if err := move(centerX+float64(total), centerY); err != nil {
			return err
		}
	}
	if err := sleepContext(ctx, 100*time.Millisecond); err != nil {
		return err
	}
	_, err := b.frame.call(ctx, "Input.dispatchMouseEvent", map[string]any{
		"type": "mouseReleased", "x": centerX + float64(total), "y": centerY, "button": "left", "buttons": 0, "clickCount": 1,
	})
	return err
}

// drainChecks 丢弃已经到达的旧交卷结果。
func (b *SMSBrowserLogin) drainChecks() {
	for {
		select {
		case <-b.checks:
		default:
			return
		}
	}
}

// waitCheck 等本次拖动的交卷结果。
func (b *SMSBrowserLogin) waitCheck(ctx context.Context) (string, error) {
	deadline := time.Now().Add(smsBrowserCheckTimeout)
	for {
		select {
		case body := <-b.checks:
			return body, nil
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return "", errors.New("teleagent: 等待滑块校验结果超时")
		}
	}
}

// waitForElement 等待页面里出现指定 id 的元素。
func (b *SMSBrowserLogin) waitForElement(ctx context.Context, id string) error {
	if b == nil || b.frame == nil {
		return errBrowserElementMissing
	}
	script := fmt.Sprintf("!!document.getElementById('%s')", id)
	deadline := time.Now().Add(smsBrowserElementTimeout)
	for {
		value, err := evalString(ctx, b.frame, script)
		if err == nil && value == "true" {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return errBrowserElementMissing
		}
		time.Sleep(smsBrowserPoll)
	}
}

// clickCodeButton 点一次「获取验证码」，用于刷新一张新的滑块图。
func (b *SMSBrowserLogin) clickCodeButton(ctx context.Context) error {
	_, err := evalString(ctx, b.frame, "(function(){var el=document.getElementById('j-msg-verifyCode');if(!el)return 'missing';if(!el.disabled)el.click();return 'ok';})()")
	return err
}

// smsPoint 是页面坐标系里的一个点。
type smsPoint struct {
	X float64
	Y float64
}

// elementCenter 计算元素中心点（用页面坐标，CDP 的 Input 事件直接吃这个坐标）。
func (b *SMSBrowserLogin) elementCenter(ctx context.Context, id string) (smsPoint, error) {
	script := fmt.Sprintf("(function(){var el=document.getElementById('%s');if(!el)return null;var rect=el.getBoundingClientRect();return {x:Math.round(rect.x+rect.width/2),y:Math.round(rect.y+rect.height/2)};})()", id)
	raw, err := evalValue(ctx, b.frame, script)
	if err != nil {
		return smsPoint{}, err
	}
	var point struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	}
	if err := json.Unmarshal(raw, &point); err != nil {
		return smsPoint{}, errBrowserElementMissing
	}
	if point.X == 0 && point.Y == 0 {
		return smsPoint{}, errBrowserElementMissing
	}
	return smsPoint{X: point.X, Y: point.Y}, nil
}

// click 在页面坐标处做一次受信点击。
func (b *SMSBrowserLogin) click(ctx context.Context, point smsPoint) error {
	if _, err := b.frame.call(ctx, "Input.dispatchMouseEvent", map[string]any{
		"type": "mousePressed", "x": point.X, "y": point.Y, "button": "left", "buttons": 1, "clickCount": 1,
	}); err != nil {
		return err
	}
	if err := sleepContext(ctx, 80*time.Millisecond); err != nil {
		return err
	}
	_, err := b.frame.call(ctx, "Input.dispatchMouseEvent", map[string]any{
		"type": "mouseReleased", "x": point.X, "y": point.Y, "button": "left", "buttons": 0, "clickCount": 1,
	})
	return err
}

// smsBrowserArguments 是启动可见浏览器的命令行。
func smsBrowserArguments(port int, profileDir, url string) []string {
	return []string{
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--user-data-dir=" + profileDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-features=msEdgeSidebarV2",
		"--disable-background-networking",
		"--disable-popup-blocking",
		"--window-size=1000,800",
		url,
	}
}

// waitForFrameTarget 等 189 登录框（独立 target）出现。
func waitForFrameTarget(ctx context.Context, base, marker string) (cdpTarget, error) {
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
					for _, target := range targets {
						if target.WebSocketDebuggerURL == "" {
							continue
						}
						if strings.Contains(target.URL, marker) {
							return target, nil
						}
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return cdpTarget{}, errors.New("teleagent: 没有等到天翼账号登录框")
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// evalValue 执行页面脚本并取回 JSON 值。
func evalValue(ctx context.Context, conn *cdpConn, expression string) (json.RawMessage, error) {
	raw, err := conn.call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  true,
	})
	if err != nil {
		return nil, err
	}
	var reply struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, err
	}
	if reply.ExceptionDetails != nil {
		return nil, fmt.Errorf("teleagent: 浏览器脚本报错: %s", reply.ExceptionDetails.Text)
	}
	return reply.Result.Value, nil
}

// evalString 执行页面脚本并把结果当字符串返回（布尔/数字也会转成文本）。
func evalString(ctx context.Context, conn *cdpConn, expression string) (string, error) {
	raw, err := evalValue(ctx, conn, expression)
	if err != nil {
		return "", err
	}
	if len(raw) == 0 {
		return "", nil
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value, nil
	}
	return string(raw), nil
}

// decodeBrowserImage 解码页面里的 data URL 图片（JPEG 或 PNG）。
func decodeBrowserImage(dataURL string) (image.Image, error) {
	payload := dataURLPayload(dataURL)
	if payload == "" {
		return nil, errCaptchaImageMismatch
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, errCaptchaImageMismatch
	}
	if bytes.HasPrefix(raw, []byte{0xFF, 0xD8}) {
		decoded, err := jpeg.Decode(bytes.NewReader(raw))
		if err != nil {
			return nil, errCaptchaImageMismatch
		}
		return decoded, nil
	}
	decoded, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, errCaptchaImageMismatch
	}
	return decoded, nil
}

// solveSMSOffset 用零均值归一化互相关（ZNCC）定位拼图块在底图上的横坐标。
//
// 拼图块的透明像素会被跳过；返回最高分与对应偏移。ZNCC 对亮度整体偏移不敏感，
// 实测峰值（0.95 左右）非常尖锐，而简单平均差会因为天空等平坦区域给出错误答案。
func solveSMSOffset(background, piece image.Image) (int, float64, error) {
	bgBounds := background.Bounds()
	pieceBounds := piece.Bounds()
	width, height := bgBounds.Dx(), bgBounds.Dy()
	pieceWidth, pieceHeight := pieceBounds.Dx(), pieceBounds.Dy()
	if pieceWidth <= 0 || pieceHeight <= 0 || width <= pieceWidth || height < pieceHeight {
		return 0, 0, errCaptchaImageMismatch
	}

	type sample struct {
		x     int
		y     int
		value float64
	}
	samples := make([]sample, 0, pieceWidth*pieceHeight)
	var sumPiece float64
	for y := 0; y < pieceHeight; y++ {
		for x := 0; x < pieceWidth; x++ {
			red, green, blue, alpha := piece.At(pieceBounds.Min.X+x, pieceBounds.Min.Y+y).RGBA()
			if alpha>>8 < 200 {
				continue
			}
			gray := (19595*float64(red>>8) + 38470*float64(green>>8) + 7471*float64(blue>>8)) / 65536
			samples = append(samples, sample{x: x, y: y, value: gray})
			sumPiece += gray
		}
	}
	if len(samples) < 100 {
		return 0, 0, errCaptchaImageMismatch
	}
	count := float64(len(samples))
	meanPiece := sumPiece / count
	var variancePiece float64
	for _, item := range samples {
		delta := item.value - meanPiece
		variancePiece += delta * delta
	}
	variancePiece /= count
	if variancePiece < 1e-9 {
		return 0, 0, errCaptchaImageMismatch
	}

	bestOffset := 0
	bestScore := math.Inf(-1)
	for offset := 0; offset <= width-pieceWidth; offset++ {
		var sumBackground, sumSquaredBackground, covariance float64
		for _, item := range samples {
			red, green, blue, _ := background.At(bgBounds.Min.X+offset+item.x, bgBounds.Min.Y+item.y).RGBA()
			gray := (19595*float64(red>>8) + 38470*float64(green>>8) + 7471*float64(blue>>8)) / 65536
			sumBackground += gray
			sumSquaredBackground += gray * gray
			covariance += (item.value - meanPiece) * gray
		}
		meanBackground := sumBackground / count
		varianceBackground := sumSquaredBackground/count - meanBackground*meanBackground
		if varianceBackground < 1e-9 {
			continue
		}
		covariance /= count
		score := covariance / math.Sqrt(variancePiece*varianceBackground)
		if score > bestScore {
			bestScore = score
			bestOffset = offset
		}
	}
	if math.IsInf(bestScore, -1) {
		return 0, 0, errCaptchaImageMismatch
	}
	return bestOffset, bestScore, nil
}

// s189CheckPassed 判断 check.do 的回包是否表示通过。
func s189CheckPassed(body string) bool {
	payload, err := s189ParseJSONP([]byte(body))
	if err != nil {
		return false
	}
	result, _ := s189ResultFields(payload)
	return result == "0"
}

// shortText 截断超长文本，便于放进日志。
func shortText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

// sleepContext 可被取消的等待。
func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
