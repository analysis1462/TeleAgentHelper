package teleagent

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
)

type signinFixture struct {
	mu          sync.Mutex
	signed      bool
	verifyFails bool
	points      float64
	claims      int
	onResult    []SignInResult
}

func (f *signinFixture) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/userCenter"):
			_, _ = w.Write([]byte("{\"code\":200,\"data\":{\"current_points_balance\":\"" + formatPoints(f.points) + "\",\"plan_display_name\":\"免费版\"}}"))
		case strings.HasSuffix(r.URL.Path, "/checkDayTask"):
			status := "fixed"
			text := "领取"
			if f.signed {
				status = ActionStatusCompleted
				text = "今日已领"
			}
			_, _ = w.Write([]byte("{\"code\":200,\"data\":{\"brand_title\":\"积分福利社\",\"activity_data\":[{\"visible\":true,\"action_text\":\"" + text + "\",\"action_status\":\"" + status + "\",\"action_type\":\"api\",\"jump_url\":\"/superCowork/sapi/user/portal/companyStore/checkTask?type=daily_login\"}]}}"))
		case strings.HasSuffix(r.URL.Path, "/checkTask"):
			f.claims++
			if !f.verifyFails {
				f.signed = true
				f.points += 100
			}
			_, _ = w.Write([]byte("{\"code\":200,\"msg\":\"success\",\"data\":[]}"))
		default:
			t.Errorf("未预期的请求 %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func formatPoints(value float64) string {
	if value == float64(int64(value)) {
		return itoa(int(value)) + ".00"
	}
	return "0.00"
}

func newSigninClient(t *testing.T, fixture *signinFixture) *Client {
	t.Helper()
	server := newTestServer(t, fixture.handler(t))
	return NewClient(ClientOptions{Origin: server.URL, Session: Session{Token: "tenantA:" + testJWT, DeviceID: "d"}})
}

func TestClaimDailyPointsClaimsOnce(t *testing.T) {
	fixture := &signinFixture{points: 6100}
	client := newSigninClient(t, fixture)
	options := SignInOptions{OnResult: func(result SignInResult) { fixture.onResult = append(fixture.onResult, result) }}

	result, err := ClaimDailyPoints(context.Background(), client, options)
	if err != nil {
		t.Fatalf("ClaimDailyPoints 失败: %v", err)
	}
	if !result.Claimed || result.Already {
		t.Fatalf("结果错误: %+v", result)
	}
	if result.PointsBefore != 6100 || result.PointsAfter != 6200 {
		t.Fatalf("积分变化错误: %+v", result)
	}
	if len(fixture.onResult) != 1 {
		t.Fatalf("OnResult 应回调一次，实际 %d", len(fixture.onResult))
	}

	again, err := ClaimDailyPoints(context.Background(), client, options)
	if err != nil {
		t.Fatalf("第二次 ClaimDailyPoints 失败: %v", err)
	}
	if !again.Already || again.Claimed {
		t.Fatalf("重复签到结果错误: %+v", again)
	}
	if fixture.claims != 1 {
		t.Fatalf("不应重复提交签到: %d", fixture.claims)
	}
}

func TestClaimDailyPointsRejectsUnconfirmed(t *testing.T) {
	fixture := &signinFixture{points: 6100, verifyFails: true}
	client := newSigninClient(t, fixture)

	_, err := ClaimDailyPoints(context.Background(), client, SignInOptions{})
	if !errors.Is(err, errSignInUnconfirmed) {
		t.Fatalf("期望 errSignInUnconfirmed，得到 %v", err)
	}
	if fixture.claims != 1 {
		t.Fatalf("claims = %d", fixture.claims)
	}
}
