package teleagent

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"
)

type keeperFixture struct {
	mu         sync.Mutex
	status     int
	startCount int
	heartbeats int
	lastID     string
}

func newKeeperClient(t *testing.T, fixture *keeperFixture) *Client {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		switch r.URL.Path {
		case "/teleagent-manager/v1/instance/list":
			_, _ = w.Write([]byte("{\"error\":{\"code\":0},\"data\":[{\"id\":\"inst-1\",\"status\":" + itoa(fixture.status) + "}]}"))
		case "/teleagent-manager/v1/instance/heartbeat":
			var payload struct {
				ID string `json:"id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			fixture.heartbeats++
			fixture.lastID = payload.ID
			_, _ = w.Write([]byte("{\"error\":{\"code\":0},\"data\":{}}"))
		case "/teleagent-manager/v1/instance/start":
			fixture.startCount++
			fixture.status = InstanceStatusRunning
			_, _ = w.Write([]byte("{\"error\":{\"code\":0},\"data\":{}}"))
		default:
			t.Errorf("未预期的请求 %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	server := newTestServer(t, handler)
	return NewClient(ClientOptions{Origin: server.URL, Session: Session{Token: "tenantA:" + testJWT, DeviceID: "d"}})
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

func TestKeepaliveStartsStoppedInstance(t *testing.T) {
	fixture := &keeperFixture{status: 0}
	client := newKeeperClient(t, fixture)
	keeper := NewKeepalive(client, KeepaliveOptions{
		AutoStart:         true,
		HeartbeatInterval: time.Minute,
		StartCooldown:     time.Minute,
	})

	if err := keeper.Step(context.Background()); err != nil {
		t.Fatalf("Step 失败: %v", err)
	}
	state := keeper.State()
	if fixture.startCount != 1 || fixture.heartbeats != 1 {
		t.Fatalf("start=%d heartbeat=%d", fixture.startCount, fixture.heartbeats)
	}
	if fixture.lastID != "inst-1" || !state.Running || state.InstanceID != "inst-1" {
		t.Fatalf("状态错误: %+v", state)
	}

	if err := keeper.Step(context.Background()); err != nil {
		t.Fatalf("第二次 Step 失败: %v", err)
	}
	if fixture.startCount != 1 {
		t.Fatalf("运行中不应重复启动: %d", fixture.startCount)
	}
}

func TestKeepaliveDoesNotStartWhenDisabled(t *testing.T) {
	fixture := &keeperFixture{status: 0}
	client := newKeeperClient(t, fixture)
	keeper := NewKeepalive(client, KeepaliveOptions{HeartbeatInterval: time.Minute})

	if err := keeper.Step(context.Background()); err != nil {
		t.Fatalf("Step 失败: %v", err)
	}
	if fixture.startCount != 0 || fixture.heartbeats != 1 {
		t.Fatalf("start=%d heartbeat=%d", fixture.startCount, fixture.heartbeats)
	}
	if keeper.State().Running {
		t.Fatal("实例应仍为停止状态")
	}
}
