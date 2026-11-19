package teleagent

import (
	"errors"
	"os"

	"github.com/uvwt/CtyunHelper/internal/storage"
)

const sessionFileName = "teleagent-session"

// ErrNoSession 表示本地没有保存过登录会话。
var ErrNoSession = errors.New("teleagent: 本机还没有 TeleAgent 登录会话")

// SaveSession 用 DPAPI 加密后写入数据目录。
func SaveSession(paths storage.Paths, session Session) error {
	if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		return err
	}
	session.Normalize()
	return storage.SaveProtectedJSON(paths, sessionFileName, session)
}

// LoadSession 读取本地登录会话。
func LoadSession(paths storage.Paths) (Session, error) {
	var session Session
	if err := storage.LoadProtectedJSON(paths, sessionFileName, &session); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Session{}, ErrNoSession
		}
		return Session{}, err
	}
	session.Normalize()
	if session.Empty() {
		return Session{}, ErrNoSession
	}
	return session, nil
}

// DeleteSession 删除本地登录会话。
func DeleteSession(paths storage.Paths) error {
	return storage.DeleteProtected(paths, sessionFileName)
}
