package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/uvwt/CtyunHelper/internal/logging"
	"github.com/uvwt/CtyunHelper/internal/storage"
	"github.com/uvwt/CtyunHelper/internal/teleagent"
)

const usage = `usage: teleagent <命令> [参数]

命令:
  login       登录并保存会话（默认弹浏览器；加 --password 走账号密码登录，加 --qr 扫码登录）
  status      查看登录账号、云电脑实例与积分状态
  refresh     续期登录态（token-refresh，免重新登录）
  signin      领取每日签到积分
  keepalive   发送云电脑保活心跳（--watch 常驻）
  daemon      常驻运行：保活云电脑 + 定时签到（推荐）
  install     设置开机自启（写入 HKCU Run）
  uninstall   移除开机自启
  instance    对云电脑实例执行 start|restart|connect
  logout      删除本机保存的会话与账号密码

示例:
  teleagent login
  teleagent login --password -u 账号 -p 密码
  teleagent login --qr
  teleagent status
  teleagent refresh
  teleagent signin
  teleagent keepalive --watch
  teleagent daemon
  teleagent install --dry-run
  teleagent instance connect`

func main() {
	defer logging.RecoverPanic("teleagent-cli")
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "错误: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return errors.New("缺少命令")
	}
	switch args[0] {
	case "login":
		return runLogin(args[1:])
	case "status":
		return runStatus(args[1:])
	case "refresh":
		return runRefresh(args[1:])
	case "signin":
		return runSignin(args[1:])
	case "keepalive":
		return runKeepalive(args[1:])
	case "instance":
		return runInstance(args[1:])
	case "daemon":
		return runDaemon(args[1:])
	case "install":
		return runInstall(args[1:])
	case "uninstall":
		return runUninstall(args[1:])
	case "logout":
		return runLogout(args[1:])
	case "help", "-h", "--help":
		fmt.Println(usage)
		return nil
	default:
		return fmt.Errorf("未知命令 %q", args[0])
	}
}

func setup() (storage.Paths, *teleagent.Client, error) {
	paths, err := storage.ResolvePaths()
	if err != nil {
		return storage.Paths{}, nil, err
	}
	session, err := teleagent.LoadSession(paths)
	if err != nil {
		if errors.Is(err, teleagent.ErrNoSession) {
			return storage.Paths{}, nil, errors.New("本机还没有登录会话，先运行: teleagent login")
		}
		return storage.Paths{}, nil, err
	}
	return paths, teleagent.NewClient(teleagent.ClientOptions{Session: session}), nil
}

func openLogger(paths storage.Paths) *logging.Logger {
	if err := os.MkdirAll(paths.LogDir, 0o700); err != nil {
		return nil
	}
	logger, err := logging.New(logging.Options{Path: filepath.Join(paths.LogDir, "teleagent.log")})
	if err != nil {
		return nil
	}
	return logger
}

func commandContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func runLogin(args []string) error {
	flags := flag.NewFlagSet("login", flag.ExitOnError)
	origin := flags.String("origin", teleagent.DefaultOrigin, "站点根地址")
	browser := flags.String("browser", "", "浏览器可执行文件路径")
	timeout := flags.Duration("timeout", teleagent.DefaultLoginTimeout, "等待登录的超时时间")
	profile := flags.String("profile", "", "独立的浏览器用户目录")
	username := flags.String("u", "", "账号")
	usernameLong := flags.String("username", "", "账号")
	password := flags.String("p", "", "密码")
	useStored := flags.Bool("password", false, "走账号密码无头登录；不带 -p 时使用已保存的账号密码")
	passwordStdin := flags.Bool("password-stdin", false, "从标准输入读取密码")
	qrLogin := flags.Bool("qr", false, "扫码登录：把二维码图片写到临时目录并等待扫码")
	noSave := flags.Bool("no-save", false, "只登录，不保存账号密码")
	_ = flags.Parse(args)

	paths, err := storage.ResolvePaths()
	if err != nil {
		return err
	}
	logger := openLogger(paths)
	if logger != nil {
		defer logger.Close()
	}
	ctx, cancel := commandContext()
	defer cancel()

	user := firstNonEmpty(*usernameLong, *username)
	pass := *password

	// 扫码登录：把二维码落到临时目录，用户用天翼账号 App 扫码。
	if *qrLogin {
		return runQRLogin(ctx, paths, *timeout)
	}

	// 只要显式给了账号密码（或要求读标准输入），就走无头登录，不弹浏览器。
	if user != "" || pass != "" || *passwordStdin || *useStored {
		if *passwordStdin {
			line, readErr := readPasswordLine()
			if readErr != nil {
				return readErr
			}
			pass = line
		}
		if user == "" || pass == "" {
			stored, storedErr := teleagent.LoadCredentials(paths)
			if storedErr != nil {
				return errors.New("需要同时提供账号与密码，例如: teleagent login -u 账号 -p 密码")
			}
			if user == "" {
				user = stored.Username
			}
			if pass == "" {
				pass = stored.Password
			}
		}
		fmt.Println("正在无头登录（自动过滑块）…")
		session, loginErr := teleagent.PasswordLogin(ctx, teleagent.PasswordLoginOptions{Username: user, Password: pass})
		if loginErr != nil {
			return loginErr
		}
		if err := teleagent.SaveSession(paths, session); err != nil {
			return err
		}
		if !*noSave {
			if err := teleagent.SaveCredentials(paths, teleagent.Credentials{Username: user, Password: pass}); err != nil {
				return err
			}
		}
		printSession(session)
		if !*noSave {
			fmt.Println("账号密码已用 DPAPI 加密保存，后续会话过期会自动重新登录。")
		}
		return nil
	}

	session, err := teleagent.LoginWithBrowser(ctx, teleagent.LoginOptions{
		Origin:      *origin,
		BrowserPath: *browser,
		ProfileDir:  *profile,
		Timeout:     *timeout,
		Logger:      logger,
		OnProgress:  func(message string) { fmt.Println("· " + message) },
	})
	if err != nil {
		return err
	}
	if err := teleagent.SaveSession(paths, session); err != nil {
		return err
	}
	fmt.Println()
	fmt.Println("登录成功，会话已加密保存。")
	printSession(session)
	return nil
}

// runQRLogin 走扫码登录：把二维码图片写到临时目录，轮询到用户扫码确认后保存会话。
func runQRLogin(ctx context.Context, paths storage.Paths, timeout time.Duration) error {
	if timeout <= 0 || timeout > teleagent.QRLoginTimeout() {
		timeout = teleagent.QRLoginTimeout()
	}
	qr, err := teleagent.StartQRLogin(ctx, teleagent.QRLoginOptions{
		OnProgress: func(message string) { fmt.Println("· " + message) },
	})
	if err != nil {
		return err
	}
	qrPath := filepath.Join(os.TempDir(), "teleagent-qr.png")
	if err := os.WriteFile(qrPath, qr.Image, 0o600); err != nil {
		return err
	}
	fmt.Println("二维码已保存到: " + qrPath)
	fmt.Println("请用天翼账号 App（小翼管家 / 天翼云盘 / 微信）扫码并在手机上确认登录。")
	fmt.Println("等待扫码…（" + qr.ExpiresAt.Local().Format("15:04:05") + " 前有效）")

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	session, err := qr.Wait(waitCtx)
	if err != nil {
		return err
	}
	if err := teleagent.SaveSession(paths, session); err != nil {
		return err
	}
	fmt.Println()
	fmt.Println("登录成功，会话已加密保存。")
	printSession(session)
	return nil
}

func printSession(session teleagent.Session) {
	fmt.Println("  账号   : " + session.AccountName)
	fmt.Println("  账号ID : " + session.AccountID)
	fmt.Println("  租户   : " + session.Tenant)
	if !session.ExpiresAt.IsZero() {
		fmt.Println("  过期   : " + session.ExpiresAt.Local().Format("2006-01-02 15:04:05"))
	}
}

// readPasswordLine 从标准输入读一行密码，去掉行尾换行。
func readPasswordLine() (string, error) {
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("读取密码: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// runRefresh 用当前 token 调续期接口，把新会话写回本机。
func runRefresh(args []string) error {
	flags := flag.NewFlagSet("refresh", flag.ExitOnError)
	_ = flags.Parse(args)
	paths, client, err := setup()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	session, refreshErr := client.RefreshSession(ctx)
	if refreshErr != nil {
		if errors.Is(refreshErr, teleagent.ErrRefreshUnavailable) {
			fmt.Println("续期不可用：" + refreshErr.Error())
			printSession(client.Session())
			return nil
		}
		return refreshErr
	}
	if saveErr := teleagent.SaveSession(paths, session); saveErr != nil {
		return saveErr
	}
	fmt.Println("登录态已续期")
	printSession(session)
	return nil
}
func runStatus(args []string) error {
	flags := flag.NewFlagSet("status", flag.ExitOnError)
	_ = flags.Parse(args)

	_, client, err := setup()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	session := client.Session()
	fmt.Println("账号   : " + session.AccountName + " (" + session.AccountID + ")")
	fmt.Println("租户   : " + session.Tenant)
	if !session.ExpiresAt.IsZero() {
		fmt.Println("登录态 : 有效至 " + session.ExpiresAt.Local().Format("2006-01-02 15:04"))
	}

	if status, err := client.ServiceStatus(ctx); err != nil {
		fmt.Println("服务   : 查询失败 - " + err.Error())
	} else {
		fmt.Println("服务   : " + status.Status)
	}

	instances, err := client.Instances(ctx)
	if err != nil {
		if errors.Is(err, teleagent.ErrSessionExpired) {
			return errors.New("会话已失效，请重新运行: teleagent login")
		}
		fmt.Println("云电脑 : 查询失败 - " + err.Error())
	} else if len(instances) == 0 {
		fmt.Println("云电脑 : 暂无实例")
	} else {
		for _, instance := range instances {
			state := "已停止"
			if instance.Running() {
				state = "运行中"
			}
			fmt.Printf("云电脑 : %s status=%d (%s)\n", instance.ID, instance.Status, state)
		}
	}

	center, err := client.UserCenter(ctx)
	if err != nil {
		if errors.Is(err, teleagent.ErrSessionExpired) {
			return errors.New("会话已失效，请重新运行: teleagent login")
		}
		fmt.Println("积分   : 查询失败 - " + err.Error())
		return nil
	}
	fmt.Printf("积分   : %.2f（套餐 %s）\n", center.Points(), center.PlanDisplayName)
	if task, err := client.DayTask(ctx); err == nil {
		if activity, ok := task.SignIn(); ok {
			state := "未领取"
			if activity.Completed() {
				state = "已领取"
			}
			fmt.Println("签到   : " + state + " - " + activity.ActionText)
		}
	}
	return nil
}

func runSignin(args []string) error {
	flags := flag.NewFlagSet("signin", flag.ExitOnError)
	watch := flags.Bool("watch", false, "常驻，按间隔循环签到")
	interval := flags.Duration("interval", teleagent.DefaultSignInInterval, "循环间隔")
	_ = flags.Parse(args)

	_, client, err := setup()
	if err != nil {
		return err
	}
	ctx, cancel := commandContext()
	defer cancel()

	options := teleagent.SignInOptions{Interval: *interval}
	if *watch {
		options.OnResult = printSignInResult
		return teleagent.RunSignInLoop(ctx, client, options)
	}
	result, err := teleagent.ClaimDailyPoints(ctx, client, options)
	if err != nil {
		if errors.Is(err, teleagent.ErrSessionExpired) {
			return errors.New("会话已失效，请重新运行: teleagent login")
		}
		return err
	}
	printSignInResult(result)
	return nil
}

func printSignInResult(result teleagent.SignInResult) {
	switch {
	case result.Claimed:
		fmt.Printf("签到成功，积分 %.2f → %.2f\n", result.PointsBefore, result.PointsAfter)
	case result.Already:
		fmt.Printf("今天已经签过了，当前积分 %.2f\n", result.PointsAfter)
	default:
		fmt.Printf("签到状态未变化（%s），当前积分 %.2f\n", result.ActionText, result.PointsAfter)
	}
}

func runKeepalive(args []string) error {
	flags := flag.NewFlagSet("keepalive", flag.ExitOnError)
	instanceID := flags.String("instance", "", "指定云电脑实例 ID")
	watch := flags.Bool("watch", false, "常驻，按间隔循环保活")
	interval := flags.Duration("interval", teleagent.DefaultHeartbeatInterval, "心跳间隔")
	start := flags.Bool("start", true, "实例未运行时自动启动")
	_ = flags.Parse(args)

	paths, client, err := setup()
	if err != nil {
		return err
	}
	logger := openLogger(paths)
	if logger != nil {
		defer logger.Close()
	}
	ctx, cancel := commandContext()
	defer cancel()

	options := teleagent.KeepaliveOptions{
		InstanceID:        *instanceID,
		HeartbeatInterval: *interval,
		AutoStart:         *start,
		Logger:            logger,
		OnState: func(state teleagent.KeepaliveState) {
			stateText := "已停止"
			if state.Running {
				stateText = "运行中"
			}
			fmt.Printf("[%s] %s %s\n", state.LastCheck.Format("15:04:05"), state.InstanceID, stateText)
		},
	}
	keeper := teleagent.NewKeepalive(client, options)
	if *watch {
		return keeper.Run(ctx)
	}
	if err := keeper.Step(ctx); err != nil {
		if errors.Is(err, teleagent.ErrSessionExpired) {
			return errors.New("会话已失效，请重新运行: teleagent login")
		}
		return err
	}
	return nil
}

func runInstance(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("用法: teleagent instance start|restart|connect [实例ID]")
	}
	action := args[0]
	flags := flag.NewFlagSet("instance", flag.ExitOnError)
	instanceID := flags.String("instance", "", "云电脑实例 ID")
	_ = flags.Parse(args[1:])

	_, client, err := setup()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	id := *instanceID
	if id == "" {
		instances, err := client.Instances(ctx)
		if err != nil {
			return err
		}
		if len(instances) == 0 {
			return errors.New("账号下没有云电脑实例")
		}
		id = instances[0].ID
	}

	switch action {
	case "start":
		if err := client.Start(ctx, id); err != nil {
			return err
		}
		fmt.Println("已发起启动：" + id)
	case "restart":
		if err := client.Restart(ctx, id); err != nil {
			return err
		}
		fmt.Println("已发起重启：" + id)
	case "connect":
		url, err := client.Connect(ctx, id)
		if err != nil {
			return err
		}
		fmt.Println(url)
	default:
		return fmt.Errorf("未知动作 %q", action)
	}
	return nil
}

func runDaemon(args []string) error {
	flags := flag.NewFlagSet("daemon", flag.ExitOnError)
	instanceID := flags.String("instance", "", "指定云电脑实例 ID")
	heartbeat := flags.Duration("heartbeat", teleagent.DefaultHeartbeatInterval, "心跳间隔")
	signinInterval := flags.Duration("signin-interval", teleagent.DefaultSignInInterval, "签到巡检间隔")
	noStart := flags.Bool("no-start", false, "实例停止时不自动拉起")
	quiet := flags.Bool("quiet", false, "不输出到控制台（开机自启用）")
	_ = flags.Parse(args)

	say := func(format string, values ...any) {
		if !*quiet {
			fmt.Printf(format, values...)
		}
	}

	paths, client, err := setup()
	if err != nil {
		return err
	}
	logger := openLogger(paths)
	if logger != nil {
		defer logger.Close()
		logger.Info("teleagent", "守护进程启动", logging.String("account", client.Session().AccountName))
	}
	ctx, cancel := commandContext()
	defer cancel()

	say("开始保活：账号 %s\n", client.Session().AccountName)

	keeper := teleagent.NewKeepalive(client, teleagent.KeepaliveOptions{
		InstanceID:        *instanceID,
		HeartbeatInterval: *heartbeat,
		AutoStart:         !*noStart,
		Logger:            logger,
		OnState: func(state teleagent.KeepaliveState) {
			stateText := "已停止"
			if state.Running {
				stateText = "运行中"
			}
			say("[%s] %s %s\n", state.LastCheck.Format("15:04:05"), state.InstanceID, stateText)
		},
	})

	errs := make(chan error, 2)
	go func() {
		errs <- runWithRelogin(ctx, paths, client, logger, say, keeper.Run)
	}()
	go func() {
		errs <- runWithRelogin(ctx, paths, client, logger, say, func(loopCtx context.Context) error {
			return teleagent.RunSignInLoop(loopCtx, client, teleagent.SignInOptions{
				Interval: *signinInterval,
				Logger:   logger,
				OnResult: func(result teleagent.SignInResult) {
					if result.Claimed {
						say("签到成功，积分 %.2f → %.2f\n", result.PointsBefore, result.PointsAfter)
					}
				},
			})
		})
	}()

	err = <-errs
	cancel()
	if logger != nil {
		if err != nil {
			logger.Warn("teleagent", "守护进程退出", logging.String("error", err.Error()))
		} else {
			logger.Info("teleagent", "守护进程退出")
		}
	}
	if errors.Is(err, teleagent.ErrSessionExpired) {
		return errors.New("会话已失效，请重新运行: teleagent login")
	}
	return err
}

// runWithRelogin 运行一个常驻循环；遇到登录态失效时用保存的账号密码自动重新登录后继续。
func runWithRelogin(ctx context.Context, paths storage.Paths, client *teleagent.Client, logger *logging.Logger, say func(string, ...any), loop func(context.Context) error) error {
	for {
		err := loop(ctx)
		if err == nil || ctx.Err() != nil {
			return err
		}
		if !errors.Is(err, teleagent.ErrSessionExpired) {
			return err
		}
		say("登录态已失效，正在尝试自动续期…\n")
		if session, refreshErr := client.RefreshSession(ctx); refreshErr == nil {
			if saveErr := teleagent.SaveSession(paths, session); saveErr != nil {
				return saveErr
			}
			say("登录态已自动续期：%s\n", session.AccountName)
			continue
		} else {
			if errors.Is(refreshErr, teleagent.ErrRefreshUnavailable) {
				say("自动续期不可用：%s\n", refreshErr.Error())
			} else {
				say("自动续期失败：%s，改为用保存的账号密码重新登录…\n", refreshErr.Error())
			}
			if logger != nil {
				logger.Warn("teleagent", "自动续期失败", logging.String("error", refreshErr.Error()))
			}
		}
		credentials, credentialErr := teleagent.LoadCredentials(paths)
		if credentialErr != nil {
			return err
		}
		if strings.TrimSpace(credentials.Password) == "" {
			say("本机没有保存的账号密码，请重新运行: teleagent login\n")
			return err
		}
		session, loginErr := teleagent.PasswordLogin(ctx, teleagent.PasswordLoginOptions{
			Username: credentials.Username,
			Password: credentials.Password,
		})
		if loginErr != nil {
			if logger != nil {
				logger.Warn("teleagent", "自动重新登录失败", logging.String("error", loginErr.Error()))
			}
			say("自动重新登录失败：%s（两分钟后重试）\n", loginErr.Error())
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Minute):
			}
			continue
		}
		if saveErr := teleagent.SaveSession(paths, session); saveErr != nil {
			return saveErr
		}
		client.SetSession(session)
		say("自动重新登录成功：%s\n", session.AccountName)
	}
}

func runInstall(args []string) error {
	flags := flag.NewFlagSet("install", flag.ExitOnError)
	executable := flags.String("exe", "", "守护进程可执行文件路径")
	dryRun := flags.Bool("dry-run", false, "只打印将写入的启动命令")
	_ = flags.Parse(args)

	target := *executable
	if target == "" {
		target = defaultDaemonExecutable()
	}
	if target == "" {
		return errors.New("无法定位可执行文件，请用 --exe 指定")
	}
	if *dryRun {
		fmt.Println("\"" + target + "\" daemon --quiet")
		return nil
	}
	if _, _, err := setup(); err != nil {
		return err
	}
	if err := installAutostart(target, "daemon --quiet"); err != nil {
		return err
	}
	fmt.Println("已设置开机自启：\"" + target + "\" daemon --quiet")
	return nil
}

func runUninstall(args []string) error {
	flags := flag.NewFlagSet("uninstall", flag.ExitOnError)
	_ = flags.Parse(args)
	if err := removeAutostart(); err != nil {
		return err
	}
	fmt.Println("已移除开机自启。")
	return nil
}

func runLogout(args []string) error {
	flags := flag.NewFlagSet("logout", flag.ExitOnError)
	_ = flags.Parse(args)

	paths, err := storage.ResolvePaths()
	if err != nil {
		return err
	}
	if err := teleagent.DeleteSession(paths); err != nil {
		return err
	}
	if err := teleagent.DeleteCredentials(paths); err != nil {
		return err
	}
	fmt.Println("已删除本机会话与账号密码。")
	return nil
}
