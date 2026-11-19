package teleagent

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/uvwt/CtyunHelper/internal/logging"
)

// DefaultSignInInterval 是签到巡检间隔。
const DefaultSignInInterval = 30 * time.Minute

// SignInResult 是一次签到巡检的结果。
type SignInResult struct {
	Claimed      bool
	Already      bool
	ActionText   string
	PointsBefore float64
	PointsAfter  float64
	At           time.Time
}

// SignInOptions 是签到巡检参数。
type SignInOptions struct {
	Interval time.Duration
	Logger   *logging.Logger
	Clock    func() time.Time
	// OnResult 在每次巡检成功结束时回调，供命令行与 UI 展示。
	OnResult func(SignInResult)
}

// ClaimDailyPoints 检查并领取当日登录积分。重复调用安全：已完成时直接返回。
func ClaimDailyPoints(ctx context.Context, client *Client, options SignInOptions) (SignInResult, error) {
	result, err := claimDailyPoints(ctx, client, options)
	if err == nil && options.OnResult != nil {
		options.OnResult(result)
	}
	return result, err
}

func claimDailyPoints(ctx context.Context, client *Client, options SignInOptions) (SignInResult, error) {
	clock := options.Clock
	if clock == nil {
		clock = time.Now
	}
	result := SignInResult{At: clock()}

	center, centerErr := client.UserCenter(ctx)
	if centerErr != nil && errors.Is(centerErr, ErrSessionExpired) {
		return result, centerErr
	}
	result.PointsBefore = center.Points()

	task, err := client.DayTask(ctx)
	if err != nil {
		return result, err
	}
	if activity, found := task.SignIn(); found {
		result.ActionText = activity.ActionText
		if activity.Completed() {
			result.Already = true
			result.PointsAfter = result.PointsBefore
			if options.Logger != nil {
				options.Logger.Debug("teleagent", "今日登录积分已领取")
			}
			return result, nil
		}
	}

	if err := client.ClaimDailyLogin(ctx); err != nil {
		return result, err
	}

	verified, err := client.DayTask(ctx)
	if err != nil {
		return result, err
	}
	after, found := verified.SignIn()
	if !found || !after.Completed() {
		return result, errSignInUnconfirmed
	}
	result.Claimed = true
	result.ActionText = after.ActionText

	latest, err := client.UserCenter(ctx)
	if err != nil {
		if errors.Is(err, ErrSessionExpired) {
			return result, err
		}
		result.PointsAfter = result.PointsBefore
	} else {
		result.PointsAfter = latest.Points()
	}

	if options.Logger != nil {
		options.Logger.Info("teleagent", "每日签到完成",
			logging.String("before", strconv.FormatFloat(result.PointsBefore, 'f', -1, 64)),
			logging.String("after", strconv.FormatFloat(result.PointsAfter, 'f', -1, 64)))
	}
	return result, nil
}

// RunSignInLoop 定时巡检签到，直到 ctx 结束。
func RunSignInLoop(ctx context.Context, client *Client, options SignInOptions) error {
	interval := options.Interval
	if interval <= 0 {
		interval = DefaultSignInInterval
	}
	for {
		result, err := ClaimDailyPoints(ctx, client, options)
		switch {
		case err == nil && result.Claimed:
			if options.Logger != nil {
				options.Logger.Info("teleagent", "已自动领取当日登录积分")
			}
		case err == nil:
		case errors.Is(err, ErrSessionExpired):
			return err
		case err != nil:
			if options.Logger != nil {
				options.Logger.Warn("teleagent", "签到失败", logging.String("error", err.Error()))
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}
