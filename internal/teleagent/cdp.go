package teleagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// cdpConn 是极简 Chrome DevTools Protocol 客户端，只实现登录捕获需要的能力。
type cdpConn struct {
	conn    *websocket.Conn
	mu      sync.Mutex
	writeMu sync.Mutex
	nextID  int64
	pending map[int64]chan cdpMessage
	done    chan struct{}

	eventMu  sync.Mutex
	eventSeq int64
	handlers map[int64]func(cdpEvent)
}

// cdpEvent 是调试协议推送的事件（无 id 的消息）。
type cdpEvent struct {
	Method string
	Params json.RawMessage
}

// subscribe 注册事件回调，返回取消订阅的函数。
func (c *cdpConn) subscribe(handler func(cdpEvent)) func() {
	c.eventMu.Lock()
	c.eventSeq++
	id := c.eventSeq
	c.handlers[id] = handler
	c.eventMu.Unlock()
	return func() {
		c.eventMu.Lock()
		delete(c.handlers, id)
		c.eventMu.Unlock()
	}
}

type cdpMessage struct {
	ID     int64           `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *cdpError       `json:"error,omitempty"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpError) Error() string {
	return fmt.Sprintf("cdp: %d %s", e.Code, e.Message)
}

func dialCDP(ctx context.Context, url string) (*cdpConn, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, url, nil)
	if err != nil {
		return nil, fmt.Errorf("teleagent: 连接浏览器调试端口: %w", err)
	}
	client := &cdpConn{
		conn:     conn,
		pending:  make(map[int64]chan cdpMessage),
		done:     make(chan struct{}),
		handlers: make(map[int64]func(cdpEvent)),
	}
	go client.readLoop()
	return client, nil
}

func (c *cdpConn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	reply := make(chan cdpMessage, 1)
	c.pending[id] = reply
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	payload := map[string]any{"id": id, "method": method}
	if params != nil {
		payload["params"] = params
	}
	c.writeMu.Lock()
	err := c.conn.WriteJSON(payload)
	c.writeMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("teleagent: 发送调试指令 %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, errors.New("teleagent: 浏览器调试连接已断开")
	case message := <-reply:
		if message.Error != nil {
			return nil, message.Error
		}
		return message.Result, nil
	}
}

func (c *cdpConn) readLoop() {
	defer close(c.done)
	for {
		var message cdpMessage
		if err := c.conn.ReadJSON(&message); err != nil {
			return
		}
		if message.ID == 0 {
			if message.Method == "" {
				continue
			}
			c.eventMu.Lock()
			handlers := make([]func(cdpEvent), 0, len(c.handlers))
			for _, handler := range c.handlers {
				handlers = append(handlers, handler)
			}
			c.eventMu.Unlock()
			event := cdpEvent{Method: message.Method, Params: message.Params}
			for _, handler := range handlers {
				go handler(event)
			}
			continue
		}
		c.mu.Lock()
		reply, ok := c.pending[message.ID]
		c.mu.Unlock()
		if !ok {
			continue
		}
		select {
		case reply <- message:
		default:
		}
	}
}

func (c *cdpConn) close() {
	_ = c.conn.Close()
}
