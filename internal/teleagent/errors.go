package teleagent

import "errors"

var (
	// ErrCredentialsRequired 表示调用账号密码登录时没有提供账号或密码。
	errCredentialsRequired = errors.New("teleagent: 需要账号与密码")
	// ErrCaptchaRejected 表示滑块校验未通过。
	errCaptchaRejected = errors.New("teleagent: 滑块校验未通过")
	// ErrCaptchaSecretKey 表示滑块接口返回的 secretKey 长度不是合法 AES 密钥。
	errCaptchaSecretKey = errors.New("teleagent: 滑块密钥长度非法")
	// ErrCaptchaImageMismatch 表示滑块图片尺寸异常，无法做模板匹配。
	errCaptchaImageMismatch = errors.New("teleagent: 滑块图片尺寸异常")
	// ErrLoginFlowMissing 表示统一认证跳转链路缺少必要参数。
	errLoginFlowMissing = errors.New("teleagent: 统一认证链路缺少 client_id 或 redirect_uri")
	// ErrNoRedirect 表示登录成功但响应里没有回调地址。
	errNoRedirect = errors.New("teleagent: 登录成功但缺少回调地址")
	// ErrLoginTokenMissing 表示回调完成后没有拿到 x-token。
	errLoginTokenMissing = errors.New("teleagent: 登录回调未写入 x-token")
	// ErrMissingInstance 表示操作云电脑时没有提供实例 ID。
	errMissingInstance = errors.New("teleagent: 缺少云电脑实例 ID")
	// ErrNoInstance 表示账号下没有可用的云电脑实例。
	errNoInstance = errors.New("teleagent: 账号下没有可用的云电脑实例")
	// ErrSignInUnconfirmed 表示提交签到后复查仍未完成。
	errSignInUnconfirmed = errors.New("teleagent: 签到结果未确认")
	// errPhoneRequired 表示短信登录没有提供合法的手机号。
	errPhoneRequired = errors.New("teleagent: 需要 11 位大陆手机号")
	// errSMSCodeRequired 表示短信登录没有提供验证码。
	errSMSCodeRequired = errors.New("teleagent: 需要短信验证码")
	// errSMSCaptchaRequired 表示 189 侧要求先过图形验证码，无法自动完成。
	errSMSCaptchaRequired = errors.New("teleagent: 该号码发送短信需要图形验证码")
	// errSMSLoginMissing 表示短信登录中间态缺失或已失效。
	errSMSLoginMissing = errors.New("teleagent: 短信登录状态缺失，请重新获取验证码")
	// errS189CaptchaPublicKey 表示内置的 189 图形验证码 RSA 公钥不可用。
	errS189CaptchaPublicKey = errors.New("teleagent: 189 图形验证码公钥解析失败")
	// errS189CaptchaDecrypt 表示 189 图形验证码回包解不开。
	errS189CaptchaDecrypt = errors.New("teleagent: 189 图形验证码回包解析失败")
	// errS189CaptchaPayload 表示 189 图形验证码接口缺少 token/图片/validate 字段。
	errS189CaptchaPayload = errors.New("teleagent: 189 图形验证码回包字段缺失")
	// errS189SignInvalid 表示 189 登录接口返回的链接或公钥不可用。
	errS189SignInvalid = errors.New("teleagent: 天翼账号登录接口返回异常")
	// err189CallbackParams 表示 189 登录成功后的回跳地址缺少 appId/paras/sign。
	err189CallbackParams = errors.New("teleagent: 189 登录回跳地址缺少票据参数")
	// err189AccountMissing 表示平台账号列表里没有可登录的主账号。
	err189AccountMissing = errors.New("teleagent: 平台账号列表为空，无法选择登录账号")
	// errSMSCaptchaSolve 表示在本机浏览器里自动过 189 图形验证码失败。
	errSMSCaptchaSolve = errors.New("teleagent: 浏览器自动过图形验证码失败")
	// errBrowserElementMissing 表示浏览器页面里没有等到需要的元素。
	errBrowserElementMissing = errors.New("teleagent: 浏览器页面缺少登录控件")
	// errQRLoginMissing 表示扫码登录中间态缺失，需要重新获取二维码。
	errQRLoginMissing = errors.New("teleagent: 扫码登录状态缺失，请重新获取二维码")
	// errQRCodeMissing 表示二维码接口没有返回可显示的图片。
	errQRCodeMissing = errors.New("teleagent: 二维码图片获取失败")
)

// ErrSMSCaptchaRequired 表示该号码发送短信需要先过图形验证码，应改走浏览器流程。
var ErrSMSCaptchaRequired = errSMSCaptchaRequired
