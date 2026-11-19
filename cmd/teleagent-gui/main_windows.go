//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/tailscale/walk"
	d "github.com/tailscale/walk/declarative"
	"github.com/uvwt/CtyunHelper/internal/logging"
	"github.com/uvwt/CtyunHelper/internal/storage"
	"github.com/uvwt/CtyunHelper/internal/teleagent"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	appTitle           = "TeleAgent 云电脑助手"
	appVersion         = "1.8.2"
	singleInstanceName = "Local\\TeleAgentHelper.SingleInstance.v1"
	autostartRunKey    = `Software\Microsoft\Windows\CurrentVersion\Run`
	autostartAppName   = "TeleAgentHelper"
	// sessionRenewThreshold 是提前续期的剩余有效期阈值：低于它就去调一次 token-refresh。
	sessionRenewThreshold = 48 * time.Hour
	// sessionRenewInterval 是检查会话剩余有效期的心跳间隔。
	sessionRenewInterval = 30 * time.Minute
)

// 固定 GUID 让 Windows Shell 按应用身份管理托盘图标。
var trayGUID = windows.GUID{
	Data1: 0x2f5b71c9,
	Data2: 0x7a3e,
	Data3: 0x4b1d,
	Data4: [8]byte{0x9c, 0x02, 0x51, 0x8e, 0x3b, 0x77, 0x0a, 0x61},
}

// 界面配色：浅灰底 + 白色卡片 + 语义色（运行中绿 / 已停止橙 / 错误红）。
var (
	colorPage  = walk.RGB(0xF3, 0xF4, 0xF6)
	colorCard  = walk.RGB(0xFF, 0xFF, 0xFF)
	colorText  = walk.RGB(0x1F, 0x23, 0x29)
	colorMuted = walk.RGB(0x6B, 0x72, 0x80)
	colorGreen = walk.RGB(0x18, 0x8A, 0x3E)
	colorRed   = walk.RGB(0xC0, 0x39, 0x2B)
	colorAmber = walk.RGB(0xB4, 0x5F, 0x06)
)

// uiFont 统一界面字体：Windows 10/11 自带微软雅黑 UI，按 pt 自动跟随 DPI。
func uiFont(size int, bold bool) d.Font {
	return d.Font{Family: "Microsoft YaHei UI", PointSize: size, Bold: bold}
}

// windowState 记录主窗口位置与大小（物理像素），下次启动恢复。
// 用物理像素而不是 96DPI 逻辑值，避免高 DPI 下 Bounds()/SetBounds() 换算不一致。
type windowState struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

func (a *app) windowStatePath() string {
	return filepath.Join(a.paths.DataDir, "ui-state.json")
}

func (a *app) loadWindowState() (windowState, bool) {
	var state windowState
	data, err := os.ReadFile(a.windowStatePath())
	if err != nil {
		return state, false
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, false
	}
	if state.Width < 860 || state.Height < 720 || state.X < -10000 || state.Y < -10000 {
		return state, false
	}
	return state, true
}

func (a *app) saveWindowState() {
	if a.window == nil {
		return
	}
	bounds := a.window.BoundsPixels()
	if bounds.Width < 400 || bounds.Height < 300 || bounds.X < -10000 || bounds.Y < -10000 {
		return
	}
	state := windowState{X: bounds.X, Y: bounds.Y, Width: bounds.Width, Height: bounds.Height}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(a.windowStatePath(), data, 0o600)
}

type app struct {
	window *walk.MainWindow
	tray   *walk.NotifyIcon

	usernameEdit *walk.LineEdit
	passwordEdit *walk.LineEdit
	remember     *walk.CheckBox

	loginButton         *walk.PushButton
	qrLoginButton       *walk.PushButton
	browserLoginButton  *walk.PushButton
	logoutButton        *walk.PushButton
	startButton         *walk.PushButton
	stopButton          *walk.PushButton
	refreshButton       *walk.PushButton
	openDesktopButton   *walk.PushButton
	signinButton        *walk.PushButton
	refreshPointsButton *walk.PushButton

	statusLabel         *walk.TextLabel
	instanceLabel       *walk.TextLabel
	instanceStatusLabel *walk.TextLabel
	heartbeatLabel      *walk.TextLabel
	checkLabel          *walk.TextLabel
	errorLabel          *walk.TextLabel
	signinLabel         *walk.TextLabel
	signinResultLabel   *walk.TextLabel
	pointsLabel         *walk.TextLabel

	instAutoStart    *walk.CheckBox
	launchAutoStart  *walk.CheckBox
	statusBarItem    *walk.StatusBarItem
	logEdit          *walk.TextEdit
	trayStatusAction *walk.Action

	logLines    []string
	accountName string

	mu            sync.Mutex
	paths         storage.Paths
	client        *teleagent.Client
	keeper        *teleagent.Keepalive
	logger        *logging.Logger
	qrLogin       *teleagent.QRLogin
	qrDlg         *walk.Dialog
	aboutDlg      *walk.Dialog
	qrView        *walk.CustomWidget
	qrBitmap      *walk.Bitmap
	qrBrush       *walk.SolidColorBrush
	qrSource      image.Image
	qrScale       int
	qrPixels      int
	qrStatus      *walk.TextLabel
	qrCancel      context.CancelFunc
	engineCancel  context.CancelFunc
	quitting      bool
	suppressEvent bool
}

func main() {
	defer logging.RecoverPanic("teleagent-gui.main")
	if err := run(); err != nil {
		walk.MsgBox(nil, appTitle, "启动失败："+err.Error(), walk.MsgBoxIconError|walk.MsgBoxOK)
	}
}

func run() error {
	paths, err := storage.ResolvePaths()
	if err != nil {
		return fmt.Errorf("解析配置目录: %w", err)
	}
	if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		return fmt.Errorf("创建数据目录: %w", err)
	}

	walkApp, err := walk.InitApp()
	if err != nil {
		return fmt.Errorf("初始化 Windows UI: %w", err)
	}
	walkApp.SetOrganizationName("uvwt")
	walkApp.SetProductName("TeleAgentHelper")

	mutex, alreadyRunning, err := acquireSingleInstance()
	if err != nil {
		return err
	}
	defer windows.CloseHandle(mutex)
	if alreadyRunning {
		if !focusExistingWindow() {
			showMessage("TeleAgent 云电脑助手已经在运行，请从系统托盘打开主界面。")
		}
		return nil
	}

	a := &app{paths: paths}
	a.logger = openLogger(paths)
	if a.logger != nil {
		defer a.logger.Close()
	}
	if err := a.create(); err != nil {
		return err
	}
	defer a.dispose()

	if state, ok := a.loadWindowState(); ok {
		_ = a.window.SetBoundsPixels(walk.Rectangle{X: state.X, Y: state.Y, Width: state.Width, Height: state.Height})
	}

	a.restoreState()
	startHidden := hasStartupFlag(os.Args[1:])
	if !startHidden || a.tray == nil {
		a.window.Show()
	}
	walkApp.Run()
	return nil
}

func hasStartupFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--startup" || arg == "-startup" || arg == "--hidden" {
			return true
		}
	}
	return false
}

func (a *app) create() error {
	caption := func(text string) d.Label {
		return d.Label{
			Text:      text,
			Font:      uiFont(8, false),
			TextColor: colorMuted,
			MinSize:   d.Size{Width: 56},
			MaxSize:   d.Size{Width: 68},
		}
	}
	value := func(target **walk.TextLabel) d.TextLabel {
		return d.TextLabel{
			AssignTo:      target,
			Text:          "-",
			Font:          uiFont(9, false),
			TextColor:     colorText,
			MinSize:       d.Size{Width: 90, Height: 20},
			StretchFactor: 1,
			TextAlignment: d.AlignHNearVCenter,
		}
	}
	card := func(stretch int, children ...d.Widget) d.Composite {
		return d.Composite{
			StretchFactor: stretch,
			Background:    d.SolidColorBrush{Color: colorCard},
			Layout: d.VBox{
				Spacing: 8,
				Margins: d.Margins{Left: 16, Top: 12, Right: 16, Bottom: 12},
			},
			Children: children,
		}
	}
	cardTitle := func(text string) d.TextLabel {
		return d.TextLabel{Text: text, Font: uiFont(11, true), TextColor: colorText}
	}
	smallButton := func(target **walk.PushButton, text string, handler walk.EventHandler, width int) d.PushButton {
		return d.PushButton{
			AssignTo:  target,
			Text:      text,
			Font:      uiFont(9, false),
			OnClicked: handler,
			MinSize:   d.Size{Width: width, Height: 32},
		}
	}

	windowSize := d.Size{Width: 960, Height: 800}
	mainWindow := d.MainWindow{
		AssignTo:       &a.window,
		Title:          appTitle + " v" + appVersion,
		Size:           windowSize,
		MinSize:        d.Size{Width: 860, Height: 720},
		Background:     d.SolidColorBrush{Color: colorPage},
		Font:           uiFont(9, false),
		Layout:         d.VBox{Margins: d.Margins{Left: 16, Top: 14, Right: 16, Bottom: 12}, Spacing: 10},
		StatusBarItems: nil,
	}
	header := d.Composite{
		Background: d.SolidColorBrush{Color: colorCard},
		Layout:     d.HBox{Spacing: 12, Margins: d.Margins{Left: 18, Top: 14, Right: 18, Bottom: 14}},
		Children: []d.Widget{
			d.Composite{
				Layout: d.VBox{Spacing: 2, MarginsZero: true},
				Children: []d.Widget{
					d.TextLabel{Text: appTitle, Font: uiFont(16, true), TextColor: colorText},
					d.TextLabel{Text: "云电脑保活 · 每日签到 · 多方式登录", Font: uiFont(9, false), TextColor: colorMuted},
				},
			},
			d.HSpacer{},
			d.Composite{
				Layout: d.VBox{Spacing: 2, MarginsZero: true},
				Children: []d.Widget{
					d.TextLabel{AssignTo: &a.statusLabel, Text: "未登录", Font: uiFont(10, true), TextColor: colorMuted, TextAlignment: d.AlignHFarVCenter},
					d.Composite{
						Layout: d.HBox{Spacing: 6, MarginsZero: true},
						Children: []d.Widget{
							d.TextLabel{Text: "v" + appVersion, Font: uiFont(8, false), TextColor: colorMuted, TextAlignment: d.AlignHFarVCenter},
							d.Label{Text: "·", Font: uiFont(8, false), TextColor: colorMuted},
							d.LinkLabel{Text: `<a href="about">关于</a>`, Font: uiFont(8, false), OnLinkActivated: func(*walk.LinkLabelLink) { a.showAbout() }},
						},
					},
				},
			},
		},
	}

	loginCard := card(0,
		d.TextLabel{Text: "账号登录", Font: uiFont(11, true), TextColor: colorText},
		d.Composite{
			Layout: d.Grid{Columns: 4, Spacing: 8},
			Children: []d.Widget{
				d.Label{Text: "账号", Font: uiFont(9, false), TextColor: colorMuted, MinSize: d.Size{Width: 36}},
				d.LineEdit{AssignTo: &a.usernameEdit, CueBanner: "手机号或账号", Font: uiFont(9, false), MinSize: d.Size{Width: 260, Height: 30}, StretchFactor: 1},
				d.Label{Text: "密码", Font: uiFont(9, false), TextColor: colorMuted, MinSize: d.Size{Width: 36}},
				d.LineEdit{AssignTo: &a.passwordEdit, PasswordMode: true, CueBanner: "登录密码", Font: uiFont(9, false), MinSize: d.Size{Width: 260, Height: 30}, StretchFactor: 1},
			},
		},
		d.Composite{
			Layout: d.HBox{Spacing: 8, MarginsZero: true},
			Children: []d.Widget{
				smallButton(&a.loginButton, "密码登录", a.onLogin, 100),
				smallButton(&a.qrLoginButton, "扫码登录", a.onQRLogin, 92),
				smallButton(&a.browserLoginButton, "浏览器登录", a.onBrowserLogin, 104),
				d.HSpacer{},
				d.CheckBox{AssignTo: &a.remember, Text: "记住账号密码（DPAPI 加密保存）", Font: uiFont(9, false), Checked: true},
				smallButton(&a.logoutButton, "退出账号", a.onLogout, 92),
			},
		},
	)

	keepCard := card(3,
		d.Composite{
			Layout: d.HBox{Spacing: 8, MarginsZero: true},
			Children: []d.Widget{
				cardTitle("云电脑保活"),
				d.HSpacer{},
				d.TextLabel{AssignTo: &a.instanceStatusLabel, Text: "-", Font: uiFont(10, true), TextColor: colorMuted, TextAlignment: d.AlignHFarVCenter},
			},
		},
		d.TextLabel{AssignTo: &a.instanceLabel, Text: "-", Font: uiFont(10, true), TextColor: colorText},
		d.Composite{
			Layout: d.HBox{Spacing: 6, MarginsZero: true},
			Children: []d.Widget{
				caption("最近心跳"), value(&a.heartbeatLabel),
				caption("最近检查"), value(&a.checkLabel),
			},
		},
		d.TextLabel{AssignTo: &a.errorLabel, Text: "", Font: uiFont(8, false), TextColor: colorRed, MinSize: d.Size{Width: 160, Height: 18}},
		d.Composite{
			Layout: d.HBox{Spacing: 8, MarginsZero: true},
			Children: []d.Widget{
				smallButton(&a.startButton, "开始保活", a.onStart, 96),
				smallButton(&a.stopButton, "停止保活", a.onStop, 96),
				smallButton(&a.refreshButton, "刷新状态", a.onRefresh, 96),
				smallButton(&a.openDesktopButton, "打开云电脑", a.onOpenDesktop, 102),
				d.HSpacer{},
			},
		},
		d.Composite{
			Layout: d.HBox{Spacing: 16, MarginsZero: true},
			Children: []d.Widget{
				d.CheckBox{AssignTo: &a.instAutoStart, Text: "实例停止时自动拉起", Font: uiFont(9, false), Checked: true},
				d.CheckBox{AssignTo: &a.launchAutoStart, Text: "开机自启（最小化到托盘）", Font: uiFont(9, false)},
				d.HSpacer{},
			},
		},
	)

	signinCard := card(2,
		d.Composite{
			Layout: d.HBox{Spacing: 8, MarginsZero: true},
			Children: []d.Widget{
				cardTitle("每日签到"),
				d.HSpacer{},
				d.TextLabel{AssignTo: &a.signinLabel, Text: "-", Font: uiFont(10, true), TextColor: colorMuted, TextAlignment: d.AlignHFarVCenter},
			},
		},
		d.Composite{
			Layout: d.HBox{Spacing: 8, MarginsZero: true},
			Children: []d.Widget{
				d.TextLabel{AssignTo: &a.pointsLabel, Text: "-", Font: uiFont(20, true), TextColor: colorText},
				d.HSpacer{},
				smallButton(&a.refreshPointsButton, "刷新积分", a.onRefreshPoints, 84),
			},
		},
		d.Label{Text: "当前积分", Font: uiFont(8, false), TextColor: colorMuted},
		d.TextLabel{AssignTo: &a.signinResultLabel, Text: "-", Font: uiFont(8, false), TextColor: colorMuted, MinSize: d.Size{Width: 160, Height: 18}},
		d.Composite{
			Layout: d.HBox{Spacing: 8, MarginsZero: true},
			Children: []d.Widget{
				smallButton(&a.signinButton, "立即签到", a.onSignin, 102),
				d.HSpacer{},
				d.Label{Text: "每 30 分钟自动巡检", Font: uiFont(8, false), TextColor: colorMuted},
			},
		},
		d.HSpacer{},
	)

	logCard := card(1,
		d.Composite{
			Layout: d.HBox{Spacing: 8, MarginsZero: true},
			Children: []d.Widget{
				cardTitle("运行日志"),
				d.HSpacer{},
				d.PushButton{Text: "复制", Font: uiFont(8, false), OnClicked: a.onCopyLog, MinSize: d.Size{Width: 62, Height: 26}},
				d.PushButton{Text: "清空", Font: uiFont(8, false), OnClicked: a.onClearLog, MinSize: d.Size{Width: 62, Height: 26}},
			},
		},
		d.TextEdit{
			AssignTo:      &a.logEdit,
			ReadOnly:      true,
			VScroll:       true,
			Font:          d.Font{Family: "Consolas", PointSize: 9},
			TextColor:     colorText,
			Background:    d.SolidColorBrush{Color: walk.RGB(0xFA, 0xFA, 0xFA)},
			StretchFactor: 1,
			MinSize:       d.Size{Width: 740, Height: 160},
		},
	)

	mainWindow.Children = []d.Widget{
		header,
		loginCard,
		d.Composite{
			Layout: d.HBox{Spacing: 12, MarginsZero: true},
			Children: []d.Widget{
				keepCard,
				signinCard,
			},
		},
		logCard,
	}

	if err := mainWindow.Create(); err != nil {
		return fmt.Errorf("创建主窗口: %w", err)
	}

	icon := walk.IconApplication()
	if resourceIcon, err := walk.NewIconFromResourceId(1); err == nil {
		icon = resourceIcon
	}
	_ = a.window.SetIcon(icon)

	if err := a.createTray(icon); err != nil {
		a.logf("系统托盘不可用（%v），关闭窗口将直接退出程序。", err)
	} else {
		a.window.SetExitOnClose(false)
		a.window.Closing().Attach(func(canceled *bool, _ walk.CloseReason) {
			a.mu.Lock()
			quitting := a.quitting
			a.mu.Unlock()
			if !quitting {
				a.saveWindowState()
				*canceled = true
				a.window.Hide()
			}
		})
	}

	a.setEngineButtons(false)
	a.startButton.SetEnabled(true)
	a.stopButton.SetEnabled(false)
	a.signinButton.SetEnabled(false)
	a.refreshPointsButton.SetEnabled(false)
	a.refreshButton.SetEnabled(false)
	a.openDesktopButton.SetEnabled(false)
	a.logoutButton.SetEnabled(false)

	if err := a.initToggles(); err != nil {
		a.logf("初始化开关状态失败：%v", err)
	}
	if a.tray != nil {
		a.appendLog("就绪。" + appTitle + " v" + appVersion + "（关闭窗口会最小化到系统托盘）")
	} else {
		a.appendLog("就绪。" + appTitle + " v" + appVersion)
	}
	return nil
}

func (a *app) initToggles() error {
	a.mu.Lock()
	a.suppressEvent = true
	a.mu.Unlock()
	a.launchAutoStart.SetChecked(autostartEnabled())
	a.mu.Lock()
	a.suppressEvent = false
	a.mu.Unlock()
	a.launchAutoStart.CheckedChanged().Attach(func() {
		a.mu.Lock()
		suppress := a.suppressEvent
		a.mu.Unlock()
		if suppress {
			return
		}
		enabled := a.launchAutoStart.Checked()
		if err := setAutostart(enabled); err != nil {
			a.appendLog("设置开机自启失败：" + err.Error())
			a.mu.Lock()
			a.suppressEvent = true
			a.mu.Unlock()
			a.launchAutoStart.SetChecked(!enabled)
			a.mu.Lock()
			a.suppressEvent = false
			a.mu.Unlock()
			return
		}
		if enabled {
			a.appendLog("已设置开机自启：" + autostartCommand())
		} else {
			a.appendLog("已取消开机自启。")
		}
	})
	return nil
}

func (a *app) createTray(icon walk.Image) error {
	tray, err := walk.NewNotifyIconWithGUID(trayGUID)
	if err != nil {
		return fmt.Errorf("创建系统托盘: %w", err)
	}
	a.tray = tray
	ready := false
	defer func() {
		if !ready {
			tray.Dispose()
			a.tray = nil
		}
	}()
	_ = tray.SetIcon(icon)
	_ = tray.SetToolTip(appTitle)

	tray.MouseDown().Attach(func(_, _ int, button walk.MouseButton) {
		if button == walk.LeftButton {
			a.showWindow()
		}
	})

	add := func(text string, fn func()) error {
		action := walk.NewAction()
		if err := action.SetText(text); err != nil {
			return err
		}
		action.Triggered().Attach(fn)
		return tray.ContextMenu().Actions().Add(action)
	}
	statusAction := walk.NewAction()
	_ = statusAction.SetText("状态：未登录")
	_ = statusAction.SetEnabled(false)
	if err := tray.ContextMenu().Actions().Add(statusAction); err != nil {
		return err
	}
	a.trayStatusAction = statusAction
	if err := tray.ContextMenu().Actions().Add(walk.NewSeparatorAction()); err != nil {
		return err
	}
	if err := add("打开主界面", a.showWindow); err != nil {
		return err
	}
	if err := add("开始保活", a.onStart); err != nil {
		return err
	}
	if err := add("停止保活", a.onStop); err != nil {
		return err
	}
	if err := add("打开云电脑", a.onOpenDesktop); err != nil {
		return err
	}
	if err := add("立即签到", a.onSignin); err != nil {
		return err
	}
	if err := tray.ContextMenu().Actions().Add(walk.NewSeparatorAction()); err != nil {
		return err
	}
	if err := add("关于", a.showAbout); err != nil {
		return err
	}
	if err := add("退出", a.quit); err != nil {
		return err
	}
	if err := tray.SetVisible(true); err != nil {
		return fmt.Errorf("显示系统托盘: %w", err)
	}
	ready = true
	return nil
}

func (a *app) dispose() {
	if a.tray != nil {
		a.tray.Dispose()
		a.tray = nil
	}
	if a.window != nil {
		a.window.Dispose()
		a.window = nil
	}
}

func (a *app) showWindow() {
	if a.window == nil {
		return
	}
	a.window.Show()
	_ = a.window.Activate()
}

func (a *app) quit() {
	a.mu.Lock()
	a.quitting = true
	a.mu.Unlock()
	a.saveWindowState()
	a.stopEngine(true)
	walk.App().Exit(0)
}

// restoreState 在窗口创建后恢复本地会话与账号密码。
func (a *app) restoreState() {
	if credentials, err := teleagent.LoadCredentials(a.paths); err == nil {
		a.usernameEdit.SetText(credentials.Username)
		a.mu.Lock()
		a.suppressEvent = false
		a.mu.Unlock()
	}

	session, err := teleagent.LoadSession(a.paths)
	switch {
	case err == nil && session.Valid(time.Now()):
		a.applySession(session)
		a.appendLog("已恢复上次登录：账号 " + displayAccount(session))
		a.startEngine()
		go a.refreshAfterLogin()
	case err == nil && !session.Empty():
		a.appendLog("上次会话已过期，正在用保存的账号密码自动登录…")
		a.usernameEdit.SetText(firstNonEmpty(session.AccountName, a.usernameEdit.Text()))
		go a.autoLogin("会话过期")
	case errors.Is(err, teleagent.ErrNoSession):
		if a.usernameEdit.Text() != "" {
			a.appendLog("检测到已保存的账号，正在自动登录…")
			go a.autoLogin("自动登录")
		} else {
			a.appendLog("还没有登录，请填写账号密码后点击“登录”。")
		}
	default:
		if err != nil {
			a.appendLog("读取本地会话失败：" + err.Error())
		}
	}
}

func (a *app) autoLogin(reason string) {
	credentials, err := teleagent.LoadCredentials(a.paths)
	if err != nil {
		a.post(func() {
			a.appendLog("本机没有保存的账号密码，请填写账号密码后点击“密码登录”，或点“浏览器登录”。")
		})
		return
	}
	a.post(func() {
		a.appendLog("尝试使用保存的账号 " + credentials.Username + " 自动登录（" + reason + "）…")
	})
	// 自动重登同样保留本地凭据，避免一次过期之后再也无法自动登录。
	a.doPasswordLogin(credentials.Username, credentials.Password, true)
}

func (a *app) onLogin() {
	username := strings.TrimSpace(a.usernameEdit.Text())
	password := a.passwordEdit.Text()
	if username == "" || password == "" {
		a.appendLog("请先填写账号和密码。")
		return
	}
	a.mu.Lock()
	if a.engineCancel != nil {
		a.mu.Unlock()
		a.appendLog("保活正在运行，如需更换账号请先点击“退出账号”。")
		return
	}
	a.mu.Unlock()
	a.doPasswordLogin(username, password, true)
}

// doPasswordLogin 在后台线程完成滑块登录，结果回到 UI 线程更新。
func (a *app) doPasswordLogin(username, password string, remember bool) {
	a.loginButton.SetEnabled(false)
	a.setStatus("正在登录（自动完成滑块验证）…")
	a.appendLog("开始登录，账号：" + username)
	go func() {
		defer logging.RecoverPanic("teleagent-gui.login")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		session, err := teleagent.PasswordLogin(ctx, teleagent.PasswordLoginOptions{Username: username, Password: password})
		if err != nil {
			a.post(func() {
				a.loginButton.SetEnabled(true)
				a.setStatus("登录失败")
				a.appendLog("登录失败：" + err.Error())
				a.reportLoginError(err)
			})
			return
		}
		saveErr := teleagent.SaveSession(a.paths, session)
		if remember {
			if err := teleagent.SaveCredentials(a.paths, teleagent.Credentials{Username: username, Password: password, SavedAt: time.Now()}); err != nil {
				a.post(func() { a.appendLog("保存账号密码失败：" + err.Error()) })
			}
		} else {
			_ = teleagent.DeleteCredentials(a.paths)
		}
		a.post(func() {
			a.loginButton.SetEnabled(true)
			a.passwordEdit.SetText("")
			if saveErr != nil {
				a.appendLog("保存会话失败：" + saveErr.Error())
			}
			a.applySession(session)
			a.appendLog("登录成功：账号 " + displayAccount(session))
			a.startEngine()
			go a.refreshAfterLogin()
		})
	}()
}

// onQRLogin 在独立窗口里显示 189 的扫码登录二维码：不打开浏览器、不需要短信，
// 用户用天翼账号 App（小翼管家 / 天翼云盘 / 微信）扫码确认后本机直接取得登录态。
func (a *app) onQRLogin() {
	a.qrLoginButton.SetEnabled(false)
	a.setStatus("正在获取扫码二维码…")
	a.appendLog("正在获取扫码登录二维码…")
	a.mu.Lock()
	if a.qrCancel != nil {
		a.qrCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.qrCancel = cancel
	a.mu.Unlock()
	go func() {
		defer logging.RecoverPanic("teleagent-gui.qrlogin")
		loginCtx, loginCancel := context.WithTimeout(ctx, 6*time.Minute)
		defer loginCancel()
		qr, err := teleagent.StartQRLogin(loginCtx, teleagent.QRLoginOptions{
			OnProgress: func(message string) {
				a.post(func() {
					a.mu.Lock()
					current := a.qrLogin
					a.mu.Unlock()
					if strings.Contains(message, "二维码已刷新") && current != nil {
						a.renderQRImage(current)
						return
					}
					a.setQRStatus(message)
					a.appendLog(message)
				})
			},
		})
		if err != nil {
			a.post(func() {
				a.finishQRLogin()
				a.setStatus("获取二维码失败")
				a.appendLog("获取扫码二维码失败：" + err.Error())
			})
			return
		}
		a.post(func() {
			a.showQRDialog(qr)
			a.setStatus("等待扫码登录…")
			a.appendLog("已生成扫码登录二维码，请用天翼账号 App 扫码（4 分钟内有效）。")
		})
		session, err := qr.Wait(loginCtx)
		if err != nil {
			cancelled := ctx.Err() != nil
			a.post(func() {
				a.finishQRLogin()
				if cancelled {
					a.appendLog("已取消扫码登录。")
					a.setStatus("未登录")
					return
				}
				a.setQRStatus("扫码登录未完成")
				a.setStatus("扫码登录未完成")
				a.appendLog("扫码登录未完成：" + err.Error())
			})
			return
		}
		saveErr := teleagent.SaveSession(a.paths, session)
		a.post(func() {
			a.closeQRDialog(true)
			a.finishQRLogin()
			if saveErr != nil {
				a.appendLog("保存会话失败：" + saveErr.Error())
			}
			a.applySession(session)
			a.appendLog("扫码登录成功：账号 " + displayAccount(session))
			a.startEngine()
			go a.refreshAfterLogin()
		})
	}()
}

// showQRDialog 在 UI 线程里创建二维码窗口，并把刷新次数与二维码绑定。
func (a *app) showQRDialog(qr *teleagent.QRLogin) {
	if qr == nil {
		return
	}
	// 关掉上一张二维码的窗口，但不要取消本次登录的 ctx。
	a.closeQRDialog(false)
	a.mu.Lock()
	a.qrLogin = qr
	a.mu.Unlock()
	spec := d.Dialog{
		AssignTo:  &a.qrDlg,
		Title:     "扫码登录 - " + appTitle,
		Size:      d.Size{Width: 320, Height: 420},
		FixedSize: true,
		Layout:    d.VBox{Spacing: 8, Margins: d.Margins{Left: 12, Top: 12, Right: 12, Bottom: 12}, Alignment: d.AlignHNearVNear},
		Children: []d.Widget{
			d.Composite{
				Layout: d.HBox{Spacing: 0, MarginsZero: true},
				Children: []d.Widget{
					d.HSpacer{},
					d.CustomWidget{AssignTo: &a.qrView, MinSize: d.Size{Width: 200, Height: 200}, PaintPixels: a.paintQR},
					d.HSpacer{},
				},
			},
			d.Label{Text: "请用天翼账号 App（小翼管家 / 天翼云盘 / 微信）扫码", TextAlignment: d.AlignCenter},
			d.TextLabel{AssignTo: &a.qrStatus, Text: "等待扫码…", TextAlignment: d.AlignHCenterVCenter},
			d.Composite{
				Layout: d.HBox{Spacing: 8, MarginsZero: true},
				Children: []d.Widget{
					d.HSpacer{},
					d.PushButton{Text: "刷新二维码", OnClicked: a.onRefreshQRCode, MinSize: d.Size{Width: 100}},
					d.PushButton{Text: "关闭", OnClicked: a.onCloseQRDialog, MinSize: d.Size{Width: 80}},
					d.HSpacer{},
				},
			},
		},
	}
	if err := spec.Create(a.window); err != nil {
		a.appendLog("打开扫码窗口失败：" + err.Error())
		return
	}
	a.qrDlg.Show()
	_ = a.qrDlg.Activate()
	a.qrDlg.Closing().Attach(func(canceled *bool, _ walk.CloseReason) {
		// 用户关掉窗口就停止轮询，避免后台一直占着一次性登录页。
		cancel := a.detachQRState()
		if cancel != nil {
			cancel()
		}
		a.qrLoginButton.SetEnabled(true)
	})
	if brush, err := walk.NewSolidColorBrush(walk.RGB(255, 255, 255)); err == nil {
		a.qrBrush = brush
	}
	a.renderQRImage(qr)
}

// scaleNearest 用最近邻把图片放大整数倍，避免插值把二维码边缘糊掉。
func scaleNearest(src image.Image, scale int) image.Image {
	if scale <= 1 {
		return src
	}
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx()*scale, b.Dy()*scale))
	for y := 0; y < b.Dy()*scale; y++ {
		sy := b.Min.Y + y/scale
		for x := 0; x < b.Dx()*scale; x++ {
			sx := b.Min.X + x/scale
			dst.Set(x, y, src.At(sx, sy))
		}
	}
	return dst
}

// paintQR 在自定义控件里把二维码画成居中正方形，避免 ImageView 在 192dpi 下按错误边界裁切。
func (a *app) paintQR(canvas *walk.Canvas, updateBounds walk.Rectangle) error {
	if a.qrBrush != nil {
		if err := canvas.FillRectanglePixels(a.qrBrush, updateBounds); err != nil {
			return err
		}
	}
	source := a.qrSource
	if source == nil || updateBounds.Width <= 0 || updateBounds.Height <= 0 {
		return nil
	}
	// updateBounds 可能只是被重绘的局部区域，统一按控件整个客户区计算，避免二维码在局部重绘时错位。
	bounds := updateBounds
	if a.qrView != nil {
		client := a.qrView.ClientBoundsPixels()
		if client.Width > 0 && client.Height > 0 {
			bounds = walk.Rectangle{X: 0, Y: 0, Width: client.Width, Height: client.Height}
		}
	}
	limit := bounds.Width
	if bounds.Height < limit {
		limit = bounds.Height
	}
	// 按当前客户区选整数放大倍数，保证二维码铺满控件且模块边缘锐利。
	scale := limit / source.Bounds().Dx()
	if scale < 1 {
		scale = 1
	}
	if scale > 8 {
		scale = 8
	}
	if a.qrBitmap == nil || a.qrScale != scale {
		if a.qrBitmap != nil {
			a.qrBitmap.Dispose()
			a.qrBitmap = nil
		}
		if bitmap, err := walk.NewBitmapFromImage(scaleNearest(source, scale)); err == nil {
			a.qrBitmap = bitmap
			a.qrScale = scale
			a.qrPixels = source.Bounds().Dx() * scale
		}
	}
	bitmap := a.qrBitmap
	if bitmap == nil {
		return nil
	}
	size := a.qrPixels
	if size <= 0 || size > limit {
		size = limit
	}
	x := bounds.X + (bounds.Width-size)/2
	y := bounds.Y + (bounds.Height-size)/2
	return canvas.DrawImageStretchedPixels(bitmap, walk.Rectangle{X: x, Y: y, Width: size, Height: size})
}

// renderQRImage 把二维码字节解码后显示到窗口里。
func (a *app) renderQRImage(qr *teleagent.QRLogin) {
	if qr == nil || len(qr.Image) == 0 {
		return
	}
	decoded, _, err := image.Decode(bytes.NewReader(qr.Image))
	if err != nil {
		a.setQRStatus("二维码图片解码失败")
		return
	}
	// 缩放放到 paintQR 里按控件实际像素做，保证窗口布局完成后也能按整数倍铺满。
	a.qrSource = decoded
	if a.qrBitmap != nil {
		a.qrBitmap.Dispose()
		a.qrBitmap = nil
	}
	a.qrScale = 0
	if a.qrView != nil {
		a.qrView.Invalidate()
	}
	a.appendLog(fmt.Sprintf("已显示二维码 %dx%d", decoded.Bounds().Dx(), decoded.Bounds().Dy()))
	a.setQRStatus("请用天翼账号 App 扫描二维码（4 分钟内有效）")
}

// onRefreshQRCode 让后台重新取一张二维码，避免用户重开窗口。
func (a *app) onRefreshQRCode() {
	a.mu.Lock()
	qr := a.qrLogin
	a.mu.Unlock()
	if qr == nil {
		a.appendLog("二维码状态已失效，请重新点击“扫码登录”。")
		return
	}
	a.setQRStatus("正在刷新二维码…")
	go func() {
		defer logging.RecoverPanic("teleagent-gui.qrrefresh")
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := qr.Refresh(ctx); err != nil {
			a.post(func() { a.setQRStatus("刷新二维码失败：" + err.Error()) })
			return
		}
		a.post(func() { a.renderQRImage(qr) })
	}()
}

// onCloseQRDialog 关闭二维码窗口并停止轮询。
func (a *app) onCloseQRDialog() {
	a.closeQRDialog(true)
}

// closeQRDialog 关闭二维码窗口；cancelWait 为真时同时取消后台轮询。
func (a *app) closeQRDialog(cancelWait bool) {
	dlg := a.qrDlg
	a.qrDlg = nil
	a.qrView = nil
	a.qrStatus = nil
	if a.qrBitmap != nil {
		a.qrBitmap.Dispose()
		a.qrBitmap = nil
	}
	if a.qrBrush != nil {
		a.qrBrush.Dispose()
		a.qrBrush = nil
	}
	var cancel context.CancelFunc
	if cancelWait {
		a.mu.Lock()
		cancel = a.qrCancel
		a.qrCancel = nil
		a.qrLogin = nil
		a.mu.Unlock()
	}
	if cancel != nil {
		cancel()
	}
	if dlg != nil {
		dlg.Close(walk.DlgCmdCancel)
	}
}

// detachQRState 清空二维码窗口引用并返回需要取消的轮询函数。
func (a *app) detachQRState() context.CancelFunc {
	a.qrDlg = nil
	a.qrView = nil
	a.qrStatus = nil
	if a.qrBitmap != nil {
		a.qrBitmap.Dispose()
		a.qrBitmap = nil
	}
	if a.qrBrush != nil {
		a.qrBrush.Dispose()
		a.qrBrush = nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	cancel := a.qrCancel
	a.qrCancel = nil
	a.qrLogin = nil
	return cancel
}

// finishQRLogin 收尾：关闭窗口（如需）、恢复按钮。
func (a *app) finishQRLogin() {
	a.qrLoginButton.SetEnabled(true)
}

// setQRStatus 在二维码窗口存在时更新状态文字。
func (a *app) setQRStatus(text string) {
	if a.qrStatus != nil {
		a.qrStatus.SetText(text)
	}
}

// onBrowserLogin 打开浏览器让用户用扫码或密码完成登录，再把浏览器里的登录态接过来。
// 短信被风控限流时这是最稳的入口。
func (a *app) onBrowserLogin() {
	a.loginButton.SetEnabled(false)
	a.setStatus("正在打开浏览器…")
	a.appendLog("正在打开浏览器，请在窗口里用“扫码登录”或账号密码完成登录（5 分钟内有效）…")
	go func() {
		defer logging.RecoverPanic("teleagent-gui.browserlogin")
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		session, err := teleagent.LoginWithBrowser(ctx, teleagent.LoginOptions{
			Logger: a.logger,
			OnProgress: func(message string) {
				a.post(func() { a.appendLog(message) })
			},
		})
		if err != nil {
			a.post(func() {
				a.loginButton.SetEnabled(true)
				a.setStatus("浏览器登录未完成")
				a.appendLog("浏览器登录失败：" + err.Error())
			})
			return
		}
		saveErr := teleagent.SaveSession(a.paths, session)
		a.post(func() {
			a.loginButton.SetEnabled(true)
			if saveErr != nil {
				a.appendLog("保存会话失败：" + saveErr.Error())
			}
			a.applySession(session)
			a.appendLog("浏览器登录成功：账号 " + displayAccount(session))
			a.startEngine()
			go a.refreshAfterLogin()
		})
	}()
}

func (a *app) reportLoginError(err error) {
	var loginErr *teleagent.LoginError
	if !errors.As(err, &loginErr) {
		return
	}
	switch loginErr.Code {
	case "10980039":
		a.appendLog(fmt.Sprintf("账号或密码不正确（已失败 %d 次）。请核对密码；密码完全正确仍被拒绝时，请改用“浏览器登录”或“扫码登录”。", loginErr.ErrCount))
	case "10980005":
		a.appendLog("服务端返回“系统繁忙”，通常是请求参数被风控拒绝，可稍后重试。")
	case "-20099":
		a.appendLog("天翼账号登录页已过期，请重新登录。")
	default:
		if loginErr.ErrCount > 0 {
			a.appendLog(fmt.Sprintf("服务端拒绝登录（错误码 %s，已失败 %d 次）。", loginErr.Code, loginErr.ErrCount))
			return
		}
		if loginErr.Message != "" {
			a.appendLog(fmt.Sprintf("天翼账号接口拒绝（错误码 %s）：%s", loginErr.Code, loginErr.Message))
		}
	}
}

func (a *app) applySession(session teleagent.Session) {
	a.ensureClient(session)
	a.accountName = displayAccount(session)
	a.usernameEdit.SetText(firstNonEmpty(session.AccountName, a.usernameEdit.Text()))
	a.setStatus("已登录：" + a.accountName)
	a.logoutButton.SetEnabled(true)
	a.qrLoginButton.SetEnabled(true)
	a.browserLoginButton.SetEnabled(true)
	a.signinButton.SetEnabled(true)
	a.refreshPointsButton.SetEnabled(true)
	a.refreshButton.SetEnabled(true)
	a.openDesktopButton.SetEnabled(true)
	a.startButton.SetEnabled(true)
	a.updateTrayTooltip("已登录")
}

func (a *app) ensureClient(session teleagent.Session) *teleagent.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client == nil {
		a.client = teleagent.NewClient(teleagent.ClientOptions{Session: session})
	} else {
		a.client.SetSession(session)
	}
	return a.client
}

func (a *app) currentClient() *teleagent.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.client
}

func (a *app) onLogout() {
	if walk.MsgBox(a.window, "退出账号", "退出后将删除本机保存的登录会话和账号密码，确定继续吗？", walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != walk.DlgCmdYes {
		return
	}
	a.stopEngine(true)
	a.mu.Lock()
	a.client = nil
	a.mu.Unlock()
	if err := teleagent.DeleteSession(a.paths); err != nil {
		a.appendLog("删除会话失败：" + err.Error())
	}
	if err := teleagent.DeleteCredentials(a.paths); err != nil {
		a.appendLog("删除账号密码失败：" + err.Error())
	}
	a.passwordEdit.SetText("")
	a.accountName = ""
	a.setStatus("未登录")
	a.logoutButton.SetEnabled(false)
	a.browserLoginButton.SetEnabled(true)
	a.qrLoginButton.SetEnabled(true)
	a.signinButton.SetEnabled(false)
	a.refreshPointsButton.SetEnabled(false)
	a.refreshButton.SetEnabled(false)
	a.openDesktopButton.SetEnabled(false)
	a.startButton.SetEnabled(false)
	a.instanceLabel.SetText("-")
	a.instanceStatusLabel.SetText("-")
	a.instanceStatusLabel.SetTextColor(colorMuted)
	a.heartbeatLabel.SetText("-")
	a.checkLabel.SetText("-")
	a.errorLabel.SetText("")
	a.signinLabel.SetText("-")
	a.pointsLabel.SetText("-")
	a.signinResultLabel.SetText("-")
	a.appendLog("已退出账号并清除本机保存的凭据。")
	a.updateTrayTooltip("未登录")
}

func (a *app) onStart() {
	a.mu.Lock()
	running := a.engineCancel != nil
	client := a.client
	a.mu.Unlock()
	if client == nil {
		a.appendLog("请先登录后再开始保活。")
		return
	}
	if running {
		a.appendLog("保活已经在运行。")
		return
	}
	a.startEngine()
}

func (a *app) onStop() {
	a.stopEngine(false)
}

func (a *app) startEngine() {
	a.mu.Lock()
	if a.engineCancel != nil || a.client == nil {
		a.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.engineCancel = cancel
	client := a.client
	a.mu.Unlock()

	autoStart := a.instAutoStart.Checked()
	keeper := teleagent.NewKeepalive(client, teleagent.KeepaliveOptions{
		HeartbeatInterval: teleagent.DefaultHeartbeatInterval,
		AutoStart:         autoStart,
		Logger:            a.logger,
		OnState: func(state teleagent.KeepaliveState) {
			a.post(func() { a.applyKeepaliveState(state) })
		},
	})

	a.mu.Lock()
	a.keeper = keeper
	a.mu.Unlock()

	go func() {
		defer logging.RecoverPanic("teleagent-gui.keepalive")
		a.runWithRelogin(ctx, "保活", keeper.Run)
	}()
	go func() {
		defer logging.RecoverPanic("teleagent-gui.renew")
		a.runRenewLoop(ctx)
	}()
	go func() {
		defer logging.RecoverPanic("teleagent-gui.signin")
		a.runWithRelogin(ctx, "自动签到", func(loopCtx context.Context) error {
			return teleagent.RunSignInLoop(loopCtx, client, teleagent.SignInOptions{
				Interval: teleagent.DefaultSignInInterval,
				Logger:   a.logger,
				OnResult: func(result teleagent.SignInResult) {
					a.post(func() { a.applySignInResult(result) })
				},
			})
		})
	}()

	a.setEngineButtons(true)
	a.appendLog("已开启保活与自动签到（心跳 30 秒，签到巡检 30 分钟）。")
	a.updateTrayTooltip("保活运行中")
}

func (a *app) stopEngine(quiet bool) {
	a.mu.Lock()
	cancel := a.engineCancel
	a.engineCancel = nil
	a.keeper = nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
		if !quiet {
			a.appendLog("已停止保活与自动签到。")
		}
	}
	a.setEngineButtons(false)
	a.updateTrayTooltip("已停止")
}

func (a *app) setEngineButtons(running bool) {
	if a.startButton != nil {
		a.startButton.SetEnabled(!running)
	}
	if a.stopButton != nil {
		a.stopButton.SetEnabled(running)
	}
}

// runWithRelogin 在会话过期时用保存的账号密码自动重新登录并继续原循环。
func (a *app) runWithRelogin(ctx context.Context, label string, loop func(context.Context) error) {
	for {
		err := loop(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil && errors.Is(err, teleagent.ErrSessionExpired) {
			a.post(func() { a.appendLog(label + "：云端登录态失效，正在尝试自动续期…") })
			if refreshErr := a.refreshSession(ctx); refreshErr == nil {
				a.post(func() { a.appendLog(label + "：登录态已自动续期") })
				continue
			} else if errors.Is(refreshErr, teleagent.ErrRefreshUnavailable) {
				a.post(func() {
					a.appendLog("自动续期不可用：" + refreshErr.Error())
				})
			} else {
				a.post(func() {
					a.appendLog("自动续期失败：" + refreshErr.Error() + "，改用保存的账号密码重新登录…")
				})
			}
			if credentials, credErr := teleagent.LoadCredentials(a.paths); credErr != nil || strings.TrimSpace(credentials.Password) == "" {
				a.post(func() {
					a.appendLog(label + "：本机没有保存的账号密码，请在主界面用“扫码登录”或“浏览器登录”重新登录。")
				})
				return
			}
			if reloginErr := a.relogin(ctx); reloginErr != nil {
				a.post(func() { a.appendLog("自动重新登录失败：" + reloginErr.Error()) })
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Minute):
				}
			}
			continue
		}
		if err != nil {
			a.post(func() { a.appendLog(label + "已停止：" + err.Error()) })
		}
		return
	}
}

// runRenewLoop 定期检查登录态剩余有效期，接近过期时自动续期，避免保活半夜断掉。
func (a *app) runRenewLoop(ctx context.Context) {
	ticker := time.NewTicker(sessionRenewInterval)
	defer ticker.Stop()
	warned := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		client := a.currentClient()
		if client == nil {
			return
		}
		session := client.Session()
		if session.ExpiresAt.IsZero() || time.Until(session.ExpiresAt) > sessionRenewThreshold {
			continue
		}
		err := a.refreshSession(ctx)
		if err == nil {
			warned = false
			a.post(func() { a.appendLog("登录态即将到期，已自动续期。") })
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, teleagent.ErrRefreshUnavailable) {
			if !warned {
				warned = true
				a.post(func() {
					a.appendLog("登录态无法自动续期（服务器已无该会话记录），当前 token 在过期前仍可用；如需长期保活请重新登录。")
				})
			}
			continue
		}
		a.post(func() { a.appendLog("自动续期失败：" + err.Error()) })
	}
}

// refreshSession 用当前 token 调续期接口，成功后落盘并热替换客户端会话。
func (a *app) refreshSession(ctx context.Context) error {
	client := a.currentClient()
	if client == nil {
		return errors.New("客户端尚未就绪")
	}
	refreshCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	session, err := client.RefreshSession(refreshCtx)
	if err != nil {
		return err
	}
	if saveErr := teleagent.SaveSession(a.paths, session); saveErr != nil {
		return saveErr
	}
	a.post(func() { a.setStatus("已登录：" + displayAccount(session)) })
	return nil
}

func (a *app) relogin(ctx context.Context) error {
	credentials, err := teleagent.LoadCredentials(a.paths)
	if err != nil {
		return err
	}
	loginCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	session, err := teleagent.PasswordLogin(loginCtx, teleagent.PasswordLoginOptions{Username: credentials.Username, Password: credentials.Password})
	if err != nil {
		return err
	}
	if err := teleagent.SaveSession(a.paths, session); err != nil {
		return err
	}
	client := a.currentClient()
	if client == nil {
		return errors.New("登录态已失效且客户端已关闭")
	}
	client.SetSession(session)
	a.post(func() { a.setStatus("已重新登录：" + displayAccount(session)) })
	return nil
}

func (a *app) onRefresh() {
	client := a.currentClient()
	if client == nil {
		a.appendLog("请先登录。")
		return
	}
	a.refreshButton.SetEnabled(false)
	go a.refreshStatus(true, false)
}

// refreshAfterLogin 在登录成功后先亮出「正在获取云电脑状态」，再并发拉取三个接口。
func (a *app) refreshAfterLogin() {
	a.post(func() { a.setStatus("正在获取云电脑状态…") })
	a.refreshStatus(false, true)
}

// statusSnapshot 是一次并发刷新拿到的三份数据。
type statusSnapshot struct {
	center    teleagent.UserCenter
	task      teleagent.DayTask
	instances []teleagent.Instance
	centerErr error
	taskErr   error
	instErr   error
}

// fetchStatusSnapshot 并发请求用户中心、签到任务和实例列表，避免串行等待。
func fetchStatusSnapshot(ctx context.Context, client *teleagent.Client) statusSnapshot {
	var snapshot statusSnapshot
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		snapshot.center, snapshot.centerErr = client.UserCenter(ctx)
	}()
	go func() {
		defer wg.Done()
		snapshot.task, snapshot.taskErr = client.DayTask(ctx)
	}()
	go func() {
		defer wg.Done()
		snapshot.instances, snapshot.instErr = client.Instances(ctx)
	}()
	wg.Wait()
	return snapshot
}

func (a *app) engineActive() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.engineCancel != nil
}

// refreshStatus 并发拉取实例与积分状态并刷新界面；manual 为 true 时把结果写进运行日志，announce 用于登录后的首次刷新。
func (a *app) refreshStatus(manual bool, announce bool) {
	client := a.currentClient()
	if client == nil {
		a.post(func() { a.refreshButton.SetEnabled(true) })
		if manual {
			a.post(func() { a.appendLog("刷新失败：客户端尚未就绪。") })
		}
		return
	}
	if manual || announce {
		a.post(func() {
			if manual {
				a.appendLog("正在刷新状态…")
			} else {
				a.appendLog("正在获取云电脑状态…")
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	snapshot := fetchStatusSnapshot(ctx, client)

	instanceText := "状态未知"
	switch {
	case snapshot.instErr != nil:
	case len(snapshot.instances) == 0:
		instanceText = "无可用云电脑"
	case snapshot.instances[0].Running():
		instanceText = "运行中"
	default:
		instanceText = "已停止"
		if a.engineActive() && a.instAutoStart != nil && a.instAutoStart.Checked() {
			instanceText = "已停止（正在自动拉起…）"
		}
	}
	signinText := "状态未知"
	if snapshot.taskErr == nil {
		if activity, found := snapshot.task.SignIn(); found {
			if activity.Completed() {
				signinText = "今日已签到"
			} else {
				signinText = "今日未签到"
			}
		} else {
			signinText = "未找到签到活动"
		}
	}

	a.post(func() {
		a.refreshButton.SetEnabled(true)
		switch {
		case snapshot.centerErr != nil:
			a.appendLog("刷新积分失败：" + snapshot.centerErr.Error())
		default:
			a.pointsLabel.SetText(fmt.Sprintf("%.2f", snapshot.center.Points()))
		}
		switch {
		case snapshot.taskErr != nil:
			a.appendLog("刷新签到状态失败：" + snapshot.taskErr.Error())
		default:
			if activity, found := snapshot.task.SignIn(); found {
				if activity.Completed() {
					a.signinLabel.SetText("今日已签到")
					a.signinLabel.SetTextColor(colorGreen)
				} else {
					a.signinLabel.SetText("今日未签到")
					a.signinLabel.SetTextColor(colorAmber)
				}
			} else {
				a.signinLabel.SetText("未找到签到活动")
				a.signinLabel.SetTextColor(colorMuted)
			}
		}
		switch {
		case snapshot.instErr != nil:
			a.appendLog("刷新云电脑状态失败：" + snapshot.instErr.Error())
		case len(snapshot.instances) == 0:
			a.instanceLabel.SetText("无可用云电脑")
			a.instanceStatusLabel.SetText("-")
			a.instanceStatusLabel.SetTextColor(colorMuted)
		default:
			instance := snapshot.instances[0]
			a.instanceLabel.SetText(instance.ID)
			if instance.Running() {
				a.instanceStatusLabel.SetText("运行中")
				a.instanceStatusLabel.SetTextColor(colorGreen)
			} else {
				a.instanceStatusLabel.SetText(instanceText)
				a.instanceStatusLabel.SetTextColor(colorAmber)
			}
		}
		if announce && a.accountName != "" {
			if snapshot.instErr == nil {
				a.setStatus("已登录：" + a.accountName + " · 云电脑 " + instanceText)
			} else {
				a.setStatus("已登录：" + a.accountName)
			}
		}
		if manual || announce {
			switch {
			case snapshot.centerErr != nil || snapshot.taskErr != nil || snapshot.instErr != nil:
				a.appendLog("刷新完成（部分失败），请查看上面的错误。")
			default:
				a.appendLog(fmt.Sprintf("状态已刷新：云电脑 %s，积分 %.2f，%s。", instanceText, snapshot.center.Points(), signinText))
			}
		}
		if errors.Is(snapshot.centerErr, teleagent.ErrSessionExpired) || errors.Is(snapshot.instErr, teleagent.ErrSessionExpired) {
			a.appendLog("登录态已失效，请重新登录或点击“退出账号”后重新登录。")
		}
	})
}

func (a *app) onOpenDesktop() {
	client := a.currentClient()
	if client == nil {
		a.appendLog("请先登录。")
		return
	}
	a.openDesktopButton.SetEnabled(false)
	a.appendLog("正在获取云电脑连接地址…")
	go a.openDesktop(client)
}

// openDesktop 在后台拿到云电脑连接地址，并用系统默认浏览器打开。
// 云电脑处于停止态时先发起启动再轮询，最多等待约 90 秒。
func (a *app) openDesktop(client *teleagent.Client) {
	defer logging.RecoverPanic("teleagent-gui.open-desktop")
	defer a.post(func() { a.openDesktopButton.SetEnabled(true) })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	instances, err := client.Instances(ctx)
	if err != nil {
		a.post(func() { a.appendLog("获取云电脑列表失败：" + err.Error()) })
		return
	}
	if len(instances) == 0 {
		a.post(func() { a.appendLog("这个账号下没有可用的云电脑。") })
		return
	}
	instance := instances[0]
	if !instance.Running() {
		a.post(func() { a.appendLog("云电脑当前未运行，正在发起启动…") })
		if err := client.Start(ctx, instance.ID); err != nil {
			a.post(func() { a.appendLog("启动云电脑失败：" + err.Error()) })
			return
		}
		running := false
		for i := 0; i < 30; i++ {
			select {
			case <-ctx.Done():
				a.post(func() { a.appendLog("打开云电脑超时（等待启动超过 3 分钟）。") })
				return
			case <-time.After(3 * time.Second):
			}
			list, err := client.Instances(ctx)
			if err != nil || len(list) == 0 {
				continue
			}
			instance = list[0]
			if instance.Running() {
				running = true
				break
			}
		}
		if !running {
			a.post(func() {
				a.appendLog("云电脑启动超时（约 90 秒），请先点“开始保活”，等状态变成运行中再试。")
			})
			return
		}
		a.post(func() { a.appendLog("云电脑已启动，正在获取连接地址…") })
	}

	link, err := client.Connect(ctx, instance.ID)
	if err != nil {
		a.post(func() { a.appendLog("获取连接地址失败：" + err.Error()) })
		return
	}
	if err := openInBrowser(link); err != nil {
		a.post(func() { a.appendLog("打开浏览器失败：" + err.Error()) })
		return
	}
	a.post(func() {
		a.appendLog("已在浏览器中打开云电脑（连接 token 有时效，请勿转发这个链接）。")
	})
}

// openInBrowser 交给系统默认浏览器打开链接，避免依赖特定浏览器路径。
func openInBrowser(link string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", link).Start()
}

// onRefreshPoints 只刷新积分余额，不触发实例与签到查询，适合频繁点击。
func (a *app) onRefreshPoints() {
	client := a.currentClient()
	if client == nil {
		a.appendLog("请先登录。")
		return
	}
	a.refreshPointsButton.SetEnabled(false)
	a.appendLog("正在刷新积分…")
	go func() {
		defer logging.RecoverPanic("teleagent-gui.refresh-points")
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		center, err := client.UserCenter(ctx)
		if err != nil && errors.Is(err, teleagent.ErrSessionExpired) {
			if refreshErr := a.refreshSession(ctx); refreshErr == nil {
				center, err = client.UserCenter(ctx)
			}
		}
		a.post(func() {
			a.refreshPointsButton.SetEnabled(true)
			if err != nil {
				a.appendLog("刷新积分失败：" + err.Error())
				if errors.Is(err, teleagent.ErrSessionExpired) {
					a.appendLog("登录态已失效，请重新登录或点击“退出账号”后重新登录。")
				}
				return
			}
			points := center.Points()
			a.pointsLabel.SetText(fmt.Sprintf("%.2f", points))
			a.signinResultLabel.SetText(fmt.Sprintf("%s 已刷新，当前积分 %.2f", time.Now().Format("01-02 15:04"), points))
			plan := strings.TrimSpace(center.PlanDisplayName)
			if plan == "" {
				plan = "未知套餐"
			}
			a.appendLog(fmt.Sprintf("积分已刷新：%.2f（%s）", points, plan))
		})
	}()
}

func (a *app) onSignin() {
	client := a.currentClient()
	if client == nil {
		a.appendLog("请先登录。")
		return
	}
	a.signinButton.SetEnabled(false)
	a.appendLog("正在领取每日签到积分…")
	go func() {
		defer logging.RecoverPanic("teleagent-gui.signin-once")
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		result, err := teleagent.ClaimDailyPoints(ctx, client, teleagent.SignInOptions{Logger: a.logger})
		a.post(func() {
			a.signinButton.SetEnabled(true)
			if err != nil {
				a.appendLog("签到失败：" + err.Error())
				return
			}
			a.applySignInResult(result)
		})
	}()
}

func (a *app) applySignInResult(result teleagent.SignInResult) {
	a.pointsLabel.SetText(fmt.Sprintf("%.2f", result.PointsAfter))
	switch {
	case result.Claimed:
		a.signinLabel.SetText("今日已签到")
		a.signinLabel.SetTextColor(colorGreen)
		a.signinResultLabel.SetText(fmt.Sprintf("%s 领取成功，积分 %.2f → %.2f", result.At.Format("01-02 15:04"), result.PointsBefore, result.PointsAfter))
		a.appendLog(a.signinResultLabel.Text())
	case result.Already:
		a.signinLabel.SetText("今日已签到")
		a.signinLabel.SetTextColor(colorGreen)
		a.signinResultLabel.SetText(fmt.Sprintf("%s 今日已领取，当前积分 %.2f", result.At.Format("01-02 15:04"), result.PointsAfter))
	default:
		a.signinLabel.SetText("今日未签到")
		a.signinLabel.SetTextColor(colorAmber)
		a.signinResultLabel.SetText(fmt.Sprintf("%s 巡检完成，积分 %.2f", result.At.Format("01-02 15:04"), result.PointsAfter))
	}
}

func (a *app) applyKeepaliveState(state teleagent.KeepaliveState) {
	if state.InstanceID != "" {
		a.instanceLabel.SetText(state.InstanceID)
	} else if a.instanceLabel.Text() == "" || a.instanceLabel.Text() == "-" {
		a.instanceLabel.SetText("（自动选择）")
	}
	switch {
	case state.Running || state.Status == teleagent.InstanceStatusRunning:
		a.instanceStatusLabel.SetText("运行中")
		a.instanceStatusLabel.SetTextColor(colorGreen)
	case state.Status != 0:
		a.instanceStatusLabel.SetText("已停止")
		a.instanceStatusLabel.SetTextColor(colorAmber)
	default:
		a.instanceStatusLabel.SetText("等待首次检查")
		a.instanceStatusLabel.SetTextColor(colorMuted)
	}
	if !state.LastHeartbeat.IsZero() {
		a.heartbeatLabel.SetText(state.LastHeartbeat.Format("15:04:05"))
	}
	if !state.LastCheck.IsZero() {
		a.checkLabel.SetText(state.LastCheck.Format("01-02 15:04:05"))
	}
	if state.LastError != "" {
		a.errorLabel.SetText(state.LastError)
	} else {
		a.errorLabel.SetText("")
	}
	if state.Running {
		a.updateTrayTooltip("保活运行中：" + state.InstanceID)
	}
}

func (a *app) setStatus(text string) {
	if a.statusLabel != nil {
		a.statusLabel.SetText(text)
		switch {
		case strings.HasPrefix(text, "已登录"):
			a.statusLabel.SetTextColor(colorGreen)
		case strings.Contains(text, "失败") || strings.Contains(text, "错误"):
			a.statusLabel.SetTextColor(colorRed)
		default:
			a.statusLabel.SetTextColor(colorMuted)
		}
	}
	if a.statusBarItem != nil {
		a.statusBarItem.SetText(appTitle + " · " + text)
	}
	if a.trayStatusAction != nil {
		_ = a.trayStatusAction.SetText("状态：" + text)
	}
}

func (a *app) updateTrayTooltip(text string) {
	if a.tray == nil {
		return
	}
	tooltip := appTitle
	if strings.TrimSpace(text) != "" {
		tooltip += " - " + text
	}
	_ = a.tray.SetToolTip(tooltip)
}

func (a *app) appendLog(text string) {
	line := time.Now().Format("2006-01-02 15:04:05") + "  " + text
	a.logLines = append(a.logLines, line)
	if len(a.logLines) > 500 {
		a.logLines = a.logLines[len(a.logLines)-500:]
	}
	if a.logEdit != nil {
		if len(a.logLines) >= 500 {
			a.logEdit.SetText(strings.Join(a.logLines, "\r\n") + "\r\n")
		} else {
			a.logEdit.AppendText(line + "\r\n")
		}
		a.logEdit.ScrollToCaret()
	}
	if a.logger != nil {
		a.logger.Info("teleagent-gui", text)
	}
}

func (a *app) onClearLog() {
	a.logLines = nil
	if a.logEdit != nil {
		a.logEdit.SetText("")
	}
}

func (a *app) onCopyLog() {
	if a.logEdit == nil {
		return
	}
	if err := walk.Clipboard().SetText(a.logEdit.Text()); err != nil {
		a.appendLog("复制日志失败：" + err.Error())
		return
	}
	a.appendLog("日志已复制到剪贴板。")
}

func (a *app) logf(format string, values ...any) {
	a.appendLog(fmt.Sprintf(format, values...))
}

// post 把后台线程的更新切回 UI 线程执行。
func (a *app) post(fn func()) {
	defer func() { _ = recover() }()
	walk.App().Synchronize(fn)
}

func displayAccount(session teleagent.Session) string {
	if name := strings.TrimSpace(session.AccountName); name != "" {
		return name
	}
	if id := strings.TrimSpace(session.AccountID); id != "" {
		return id
	}
	return "（未知账号）"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func openLogger(paths storage.Paths) *logging.Logger {
	if err := os.MkdirAll(paths.LogDir, 0o700); err != nil {
		return nil
	}
	logger, err := logging.New(logging.Options{Path: filepath.Join(paths.LogDir, "teleagent-gui.log")})
	if err != nil {
		return nil
	}
	return logger
}

var (
	user32DLL            = windows.NewLazySystemDLL("user32.dll")
	procFindWindowW      = user32DLL.NewProc("FindWindowW")
	procShowWindow       = user32DLL.NewProc("ShowWindow")
	procSetForegroundWnd = user32DLL.NewProc("SetForegroundWindow")
	procMessageBoxW      = user32DLL.NewProc("MessageBoxW")
)

// focusExistingWindow 把已经在运行的实例窗口唤到前台，并返回是否找到。
func focusExistingWindow() bool {
	title, err := windows.UTF16PtrFromString(appTitle)
	if err != nil {
		return false
	}
	hwnd, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(title)))
	if hwnd == 0 {
		return false
	}
	const swRestore = 9
	_, _, _ = procShowWindow.Call(hwnd, swRestore)
	_, _, _ = procSetForegroundWnd.Call(hwnd)
	return true
}

// showMessage 直接用 Win32 MessageBox，避免 UI 框架尚未启动时无法提示。
func showMessage(text string) {
	captionPtr, _ := windows.UTF16PtrFromString(appTitle)
	textPtr, err := windows.UTF16PtrFromString(text)
	if err != nil || textPtr == nil {
		return
	}
	const mbOK, mbIconInformation = 0x00000000, 0x00000040
	_, _, _ = procMessageBoxW.Call(0, uintptr(unsafe.Pointer(textPtr)), uintptr(unsafe.Pointer(captionPtr)), mbOK|mbIconInformation)
}

func acquireSingleInstance() (windows.Handle, bool, error) {
	name, err := windows.UTF16PtrFromString(singleInstanceName)
	if err != nil {
		return 0, false, fmt.Errorf("编码互斥量名称: %w", err)
	}
	handle, err := windows.CreateMutex(nil, false, name)
	if err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return handle, true, nil
		}
		return 0, false, fmt.Errorf("创建单实例互斥量: %w", err)
	}
	return handle, false, nil
}

func executablePath() string {
	executable, err := os.Executable()
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return executable
	}
	return resolved
}

func autostartCommand() string {
	executable := executablePath()
	if executable == "" {
		return ""
	}
	return `"` + executable + `" --startup`
}

func autostartEnabled() bool {
	command := autostartCommand()
	if command == "" {
		return false
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, autostartRunKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()
	value, _, err := key.GetStringValue(autostartAppName)
	if err != nil {
		return false
	}
	return value == command
}

func setAutostart(enabled bool) error {
	key, err := registry.OpenKey(registry.CURRENT_USER, autostartRunKey, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	if enabled {
		command := autostartCommand()
		if command == "" {
			return errors.New("定位当前可执行文件失败")
		}
		return key.SetStringValue(autostartAppName, command)
	}
	if err := key.DeleteValue(autostartAppName); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}
