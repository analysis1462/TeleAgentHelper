//go:build windows

package main

import (
	"github.com/tailscale/walk"
	d "github.com/tailscale/walk/declarative"
)

const (
	teleagentProjectURL  = "https://github.com/analysis1462/TeleAgentHelper"
	teleagentUpstreamURL = "https://github.com/uvwt/CtyunHelper"
)

const aboutDisclaimer = `TeleAgentHelper 是第三方非官方辅助工具，与 TeleAgent、中国电信及其关联公司不存在隶属、授权、合作或背书关系。

使用本软件可能违反平台服务条款，可能导致账号受限、积分取消、云电脑回收等后果。请在遵守当地法律法规及平台条款的前提下使用，并自行承担全部风险。

本软件按“现状”提供，不保证功能持续可用，也不保证与平台接口长期兼容；因使用本软件造成的账号、数据或设备损失，由使用者自行承担。

账号密码仅保存在本机，使用 Windows DPAPI 加密。请只在可信设备上使用，并妥善保管账号信息。`

// showAbout 打开“关于”窗口；再次点击时把已有窗口提到前台。
func (a *app) showAbout() {
	if a.aboutDlg != nil {
		a.aboutDlg.Show()
		_ = a.aboutDlg.Activate()
		return
	}
	var closeButton *walk.PushButton
	openLink := func(link *walk.LinkLabelLink) {
		if err := openInBrowser(link.URL()); err != nil {
			a.appendLog("打开链接失败：" + err.Error())
		}
	}
	openProject := func() {
		if err := openInBrowser(teleagentProjectURL); err != nil {
			a.appendLog("打开项目主页失败：" + err.Error())
		}
	}
	copyProject := func() {
		if err := walk.Clipboard().SetText(teleagentProjectURL); err != nil {
			a.appendLog("复制项目地址失败：" + err.Error())
			return
		}
		a.appendLog("项目地址已复制到剪贴板。")
	}
	spec := d.Dialog{
		AssignTo:   &a.aboutDlg,
		Title:      "关于 " + appTitle,
		Size:       d.Size{Width: 680, Height: 470},
		FixedSize:  true,
		Background: d.SolidColorBrush{Color: colorCard},
		Font:       uiFont(9, false),
		Layout:     d.VBox{Spacing: 10, Alignment: d.AlignHNearVCenter, Margins: d.Margins{Left: 22, Top: 18, Right: 22, Bottom: 16}},
		Children: []d.Widget{
			d.Composite{
				Layout:  d.HBox{Spacing: 12, MarginsZero: true},
				MinSize: d.Size{Width: 620, Height: 32},
				Children: []d.Widget{
					d.TextLabel{Text: appTitle, Font: uiFont(18, true), TextColor: colorText},
					d.HSpacer{},
					d.TextLabel{Text: "v" + appVersion, Font: uiFont(10, false), TextColor: colorMuted, TextAlignment: d.AlignHFarVCenter},
				},
			},
			d.TextLabel{Text: "第三方 TeleAgent 云电脑辅助工具 · 云电脑保活 / 每日签到 / 多方式登录", Font: uiFont(9, false), TextColor: colorMuted},
			d.Composite{
				Layout:  d.VBox{Spacing: 4, Alignment: d.AlignHNearVCenter, MarginsZero: true},
				MinSize: d.Size{Width: 620, Height: 44},
				Children: []d.Widget{
					d.Composite{
						Layout:  d.HBox{Spacing: 8, MarginsZero: true},
						MinSize: d.Size{Width: 620, Height: 20},
						Children: []d.Widget{
							d.Label{Text: "项目地址", Font: uiFont(9, false), TextColor: colorMuted, MinSize: d.Size{Width: 64}},
							d.LinkLabel{Text: `<a href="` + teleagentProjectURL + `">` + teleagentProjectURL + `</a>`, Font: uiFont(9, false), OnLinkActivated: openLink},
						},
					},
					d.Composite{
						Layout:  d.HBox{Spacing: 8, MarginsZero: true},
						MinSize: d.Size{Width: 620, Height: 20},
						Children: []d.Widget{
							d.Label{Text: "上游项目", Font: uiFont(9, false), TextColor: colorMuted, MinSize: d.Size{Width: 64}},
							d.LinkLabel{Text: `<a href="` + teleagentUpstreamURL + `">` + teleagentUpstreamURL + `</a>`, Font: uiFont(9, false), OnLinkActivated: openLink},
						},
					},
				},
			},
			d.TextLabel{Text: "免责声明", Font: uiFont(10, true), TextColor: colorText},
			d.TextEdit{
				ReadOnly:      true,
				VScroll:       true,
				Text:          aboutDisclaimer,
				Font:          uiFont(9, false),
				TextColor:     colorText,
				Background:    d.SolidColorBrush{Color: walk.RGB(0xFA, 0xFA, 0xFA)},
				StretchFactor: 1,
				MinSize:       d.Size{Width: 620, Height: 150},
			},
			d.TextLabel{Text: "本项目原创部分采用 MIT License，具体范围见仓库 LICENSE 文件；上游代码权利归原作者所有。", Font: uiFont(8, false), TextColor: colorMuted},
			d.Composite{
				Layout:  d.HBox{Spacing: 8, MarginsZero: true},
				MinSize: d.Size{Width: 620, Height: 32},
				Children: []d.Widget{
					d.PushButton{Text: "打开项目主页", Font: uiFont(9, false), OnClicked: openProject, MinSize: d.Size{Width: 112, Height: 32}},
					d.PushButton{Text: "复制项目地址", Font: uiFont(9, false), OnClicked: copyProject, MinSize: d.Size{Width: 112, Height: 32}},
					d.HSpacer{},
					d.PushButton{AssignTo: &closeButton, Text: "关闭", Font: uiFont(9, false), Default: true, OnClicked: func() { a.closeAbout() }, MinSize: d.Size{Width: 84, Height: 32}},
				},
			},
		},
		CancelButton:  &closeButton,
		DefaultButton: &closeButton,
	}
	if err := spec.Create(a.window); err != nil {
		a.appendLog("打开关于窗口失败：" + err.Error())
		return
	}
	a.aboutDlg.Closing().Attach(func(canceled *bool, _ walk.CloseReason) {
		a.aboutDlg = nil
	})
	a.aboutDlg.Show()
	_ = a.aboutDlg.Activate()
}

func (a *app) closeAbout() {
	if a.aboutDlg != nil {
		a.aboutDlg.Close(walk.DlgCmdCancel)
	}
}
