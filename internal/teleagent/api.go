package teleagent

import (
	"context"
	"strconv"
	"strings"
)

// 管理面实例状态：100 为运行中（抓包实测值）。
const InstanceStatusRunning = 100

// ActionStatusCompleted 表示该活动今天已经完成。
const ActionStatusCompleted = "completed"

// Instance 是云电脑实例。
type Instance struct {
	ID             string `json:"id"`
	UID            string `json:"uid"`
	Status         int    `json:"status"`
	StatusReason   int    `json:"statusReason"`
	ConnectTimeout int    `json:"connectTimeout"`
}

// Running 判断实例是否处于运行中。
func (i Instance) Running() bool { return i.Status == InstanceStatusRunning }

// ServiceStatus 是管理面健康状态。
type ServiceStatus struct {
	Status string `json:"status"`
}

// DayTaskActivity 是"积分福利社"里的一项活动。
type DayTaskActivity struct {
	Visible            bool   `json:"visible"`
	ActivityTitle      string `json:"activity_title"`
	ActivitySubtitle   string `json:"activity_subtitle"`
	ActivityUserTarget string `json:"activity_user_target"`
	ActionText         string `json:"action_text"`
	ActionStatus       string `json:"action_status"`
	ActionType         string `json:"action_type"`
	JumpURL            string `json:"jump_url"`
	IsMainActivity     bool   `json:"is_main_activity"`
}

// Completed 判断活动今天是否已完成。
func (a DayTaskActivity) Completed() bool {
	return strings.EqualFold(strings.TrimSpace(a.ActionStatus), ActionStatusCompleted)
}

// SignInActivity 判断是否为每日登录签到活动。
func (a DayTaskActivity) SignInActivity() bool {
	return strings.Contains(a.JumpURL, "daily_login") || strings.Contains(a.ActionText, "已领") || strings.Contains(a.ActionText, "领取")
}

// DayTask 是 checkDayTask 的返回结构。
type DayTask struct {
	BrandTitle string            `json:"brand_title"`
	Activities []DayTaskActivity `json:"activity_data"`
}

// SignIn 找出每日签到活动。
func (t DayTask) SignIn() (DayTaskActivity, bool) {
	for _, activity := range t.Activities {
		if activity.SignInActivity() {
			return activity, true
		}
	}
	return DayTaskActivity{}, false
}

// CenterActivity 是积分中心的条目。
type CenterActivity struct {
	Name                    string `json:"name"`
	ExpireDays              int    `json:"expire_days"`
	RemainingActivityPoints string `json:"remaining_activity_points"`
	TotalActivityPoints     int    `json:"total_activity_points"`
	UsagePercent            string `json:"usage_percent"`
	Balance                 int    `json:"balance"`
}

// UserCenter 是积分中心接口的返回结构。
type UserCenter struct {
	PlanDisplayName      string           `json:"plan_display_name"`
	CurrentPointsBalance string           `json:"current_points_balance"`
	MaxConcurrentTasks   int              `json:"max_concurrent_tasks"`
	Activities           []CenterActivity `json:"activities"`
	UserRoleType         string           `json:"user_role_type"`
	WelfareClubEnabled   bool             `json:"welfare_club_enabled"`
}

// Points 解析当前积分余额。
func (u UserCenter) Points() float64 {
	value, err := strconv.ParseFloat(strings.TrimSpace(u.CurrentPointsBalance), 64)
	if err != nil {
		return 0
	}
	return value
}

// UserInfo 是账号信息。
type UserInfo struct {
	AccountID   string `json:"accountId"`
	UserID      string `json:"userId"`
	AccountName string `json:"accountName"`
	UserName    string `json:"userName"`
	Phone       string `json:"phone"`
}

// ConnectResult 是 connect 接口返回。
type ConnectResult struct {
	ConnectURL string `json:"connectURL"`
}

// ServiceStatus 查询管理面健康状态。
func (c *Client) ServiceStatus(ctx context.Context) (ServiceStatus, error) {
	var status ServiceStatus
	if err := c.do(ctx, "GET", "/teleagent-manager/v1/service/status", nil, &status); err != nil {
		return ServiceStatus{}, err
	}
	return status, nil
}

// Instances 列出云电脑实例。
func (c *Client) Instances(ctx context.Context) ([]Instance, error) {
	var instances []Instance
	if err := c.do(ctx, "GET", "/teleagent-manager/v1/instance/list", nil, &instances); err != nil {
		return nil, err
	}
	return instances, nil
}

// Heartbeat 续约实例，等价于网页端定时保活。
func (c *Client) Heartbeat(ctx context.Context, id string) error {
	return c.instanceAction(ctx, "heartbeat", id)
}

// Start 启动实例。
func (c *Client) Start(ctx context.Context, id string) error {
	return c.instanceAction(ctx, "start", id)
}

// Restart 重启实例。
func (c *Client) Restart(ctx context.Context, id string) error {
	return c.instanceAction(ctx, "restart", id)
}

// Refresh 刷新实例。
func (c *Client) Refresh(ctx context.Context, id string) error {
	return c.instanceAction(ctx, "refresh", id)
}

// Connect 申请远程桌面连接地址。
func (c *Client) Connect(ctx context.Context, id string) (string, error) {
	var result ConnectResult
	body := map[string]string{"id": id}
	if err := c.do(ctx, "POST", "/teleagent-manager/v1/instance/connect", body, &result); err != nil {
		return "", err
	}
	return result.ConnectURL, nil
}

func (c *Client) instanceAction(ctx context.Context, action, id string) error {
	if strings.TrimSpace(id) == "" {
		return errMissingInstance
	}
	body := map[string]string{"id": id}
	return c.do(ctx, "POST", "/teleagent-manager/v1/instance/"+action, body, nil)
}

// DayTask 拉取积分福利社活动列表。
func (c *Client) DayTask(ctx context.Context) (DayTask, error) {
	var task DayTask
	const path = "/superCowork/sapi/user/portal/companyStore/v3/checkDayTask?theme=dark"
	if err := c.do(ctx, "GET", path, nil, &task); err != nil {
		return DayTask{}, err
	}
	return task, nil
}

// ClaimDailyLogin 领取每日登录积分。
func (c *Client) ClaimDailyLogin(ctx context.Context) error {
	return c.do(ctx, "GET", "/superCowork/sapi/user/portal/companyStore/checkTask?type=daily_login", nil, nil)
}

// UserCenter 拉取积分中心（余额、套餐、活动）。
func (c *Client) UserCenter(ctx context.Context) (UserCenter, error) {
	var center UserCenter
	if err := c.do(ctx, "GET", "/superCowork/sapi/user/portal/companyStore/userCenter", nil, &center); err != nil {
		return UserCenter{}, err
	}
	return center, nil
}

// UserInfo 拉取账号信息。
func (c *Client) UserInfo(ctx context.Context) (UserInfo, error) {
	var info UserInfo
	if err := c.do(ctx, "GET", "/superCowork/sapi/cowork/user_info", nil, &info); err != nil {
		return UserInfo{}, err
	}
	return info, nil
}
