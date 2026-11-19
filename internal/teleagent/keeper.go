package teleagent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/uvwt/CtyunHelper/internal/logging"
)

const (
	// DefaultHeartbeatInterval 与网页端一致：30 秒一次心跳。
	DefaultHeartbeatInterval = 30 * time.Second
	// DefaultStatusInterval 是实例状态复查间隔。
	DefaultStatusInterval = 5 * time.Minute
	// DefaultStartCooldown 是两次自动启动之间的最小间隔。
	DefaultStartCooldown = 5 * time.Minute
	maxKeepaliveBackoff  = 5 * time.Minute
)

// KeepaliveState 是保活循环对外暴露的快照。
type KeepaliveState struct {
	InstanceID    string
	Status        int
	Running       bool
	LastHeartbeat time.Time
	LastCheck     time.Time
	StartAttempts int
	LastError     string
}

// KeepaliveOptions 是保活循环参数。
type KeepaliveOptions struct {
	InstanceID        string
	HeartbeatInterval time.Duration
	StatusInterval    time.Duration
	StartCooldown     time.Duration
	// AutoStart 允许在实例已停止时自动发起启动。
	AutoStart bool
	Logger    *logging.Logger
	Clock     func() time.Time
	OnState   func(KeepaliveState)
}

// Keepalive 定时续约云电脑实例，实例掉线时尝试拉起。
type Keepalive struct {
	client *Client
	opts   KeepaliveOptions

	mu               sync.Mutex
	state            KeepaliveState
	instance         *Instance
	instanceAt       time.Time
	lastStartAttempt time.Time
}

// NewKeepalive 构造保活循环。
func NewKeepalive(client *Client, options KeepaliveOptions) *Keepalive {
	if options.HeartbeatInterval <= 0 {
		options.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if options.StatusInterval <= 0 {
		options.StatusInterval = DefaultStatusInterval
	}
	if options.StartCooldown <= 0 {
		options.StartCooldown = DefaultStartCooldown
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	return &Keepalive{client: client, opts: options}
}

// State 返回当前状态快照。
func (k *Keepalive) State() KeepaliveState {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.state
}

// Step 执行一次"复查状态 + 必要时启动 + 心跳"。
func (k *Keepalive) Step(ctx context.Context) error {
	now := k.opts.Clock()
	instance, err := k.resolve(ctx, now)
	if err != nil {
		k.recordError(err)
		return err
	}
	if !instance.Running() && k.opts.AutoStart && k.canStart(now) {
		k.lastStartAttempt = now
		k.mu.Lock()
		k.state.StartAttempts++
		k.mu.Unlock()
		if startErr := k.client.Start(ctx, instance.ID); startErr != nil {
			k.recordError(startErr)
			return fmt.Errorf("teleagent: 启动实例 %s: %w", instance.ID, startErr)
		}
		k.logf(logging.LevelWarn, "云电脑未运行，已发起启动", instance.ID, instance.Status)
		instance.Status = InstanceStatusRunning
		k.instance = instance
	}
	if err := k.client.Heartbeat(ctx, instance.ID); err != nil {
		k.recordError(err)
		return err
	}
	k.mu.Lock()
	k.state.InstanceID = instance.ID
	k.state.Status = instance.Status
	k.state.Running = instance.Running()
	k.state.LastHeartbeat = now
	k.state.LastCheck = now
	k.state.LastError = ""
	state := k.state
	k.mu.Unlock()
	if k.opts.OnState != nil {
		k.opts.OnState(state)
	}
	return nil
}

// Run 持续保活，遇到登录失效时返回 ErrSessionExpired 让上层重新登录。
func (k *Keepalive) Run(ctx context.Context) error {
	interval := k.opts.HeartbeatInterval
	backoff := interval
	for {
		err := k.Step(ctx)
		wait := interval
		switch {
		case err == nil:
			backoff = interval
		case errors.Is(err, ErrSessionExpired):
			return err
		default:
			wait = backoff
			if backoff < maxKeepaliveBackoff {
				backoff *= 2
				if backoff > maxKeepaliveBackoff {
					backoff = maxKeepaliveBackoff
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

func (k *Keepalive) resolve(ctx context.Context, now time.Time) (*Instance, error) {
	k.mu.Lock()
	cached := k.instance
	cachedAt := k.instanceAt
	k.mu.Unlock()
	if cached != nil && now.Sub(cachedAt) < k.opts.StatusInterval {
		return cached, nil
	}
	instances, err := k.client.Instances(ctx)
	if err != nil {
		return nil, err
	}
	if len(instances) == 0 {
		return nil, errNoInstance
	}
	instance := instances[0]
	if id := k.opts.InstanceID; id != "" {
		found := false
		for _, candidate := range instances {
			if candidate.ID == id {
				instance, found = candidate, true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("teleagent: 实例 %s 不在账号的实例列表中", id)
		}
	}
	k.mu.Lock()
	k.instance = &instance
	k.instanceAt = now
	k.mu.Unlock()
	return &instance, nil
}

func (k *Keepalive) canStart(now time.Time) bool {
	if k.lastStartAttempt.IsZero() {
		return true
	}
	return now.Sub(k.lastStartAttempt) >= k.opts.StartCooldown
}

func (k *Keepalive) recordError(err error) {
	k.mu.Lock()
	k.state.LastError = err.Error()
	k.state.LastCheck = k.opts.Clock()
	k.mu.Unlock()
	if k.opts.Logger != nil {
		k.opts.Logger.Warn("teleagent", "保活失败", logging.String("error", err.Error()))
	}
	if k.opts.OnState != nil {
		k.mu.Lock()
		state := k.state
		k.mu.Unlock()
		k.opts.OnState(state)
	}
}

func (k *Keepalive) logf(level logging.Level, message, instanceID string, status int) {
	if k.opts.Logger == nil {
		return
	}
	fields := []logging.Field{logging.String("instance", instanceID)}
	if status != 0 {
		fields = append(fields, logging.Int("status", status))
	}
	switch level {
	case logging.LevelWarn:
		k.opts.Logger.Warn("teleagent", message, fields...)
	default:
		k.opts.Logger.Info("teleagent", message, fields...)
	}
}
