package teleagent

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/uvwt/CtyunHelper/internal/storage"
)

const (
	credentialFileName = "teleagent-credential"
	// phoneFileName 记录最近一次短信登录使用的手机号，用于下次预填。
	phoneFileName = "teleagent-phone"
)

// ErrNoCredentials 表示本机没有保存过账号密码。
var ErrNoCredentials = errors.New("teleagent: 本机没有保存 TeleAgent 账号密码")

// Credentials 是本机保存的登录凭据，用 DPAPI 加密落盘。
type Credentials struct {
	Username string    `json:"username"`
	Password string    `json:"password"`
	SavedAt  time.Time `json:"saved_at"`
}

// Empty 判断凭据是否为空。
func (c Credentials) Empty() bool { return c.Username == "" || c.Password == "" }

// SaveCredentials 保存账号密码。
func SaveCredentials(paths storage.Paths, credentials Credentials) error {
	if credentials.Empty() {
		return errCredentialsRequired
	}
	if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		return err
	}
	if credentials.SavedAt.IsZero() {
		credentials.SavedAt = time.Now()
	}
	return storage.SaveProtectedJSON(paths, credentialFileName, credentials)
}

// LoadCredentials 读取本机保存的账号密码。
func LoadCredentials(paths storage.Paths) (Credentials, error) {
	var credentials Credentials
	if err := storage.LoadProtectedJSON(paths, credentialFileName, &credentials); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Credentials{}, ErrNoCredentials
		}
		return Credentials{}, err
	}
	if credentials.Empty() {
		return Credentials{}, ErrNoCredentials
	}
	return credentials, nil
}

// DeleteCredentials 删除本机保存的账号密码。
func DeleteCredentials(paths storage.Paths) error {
	return storage.DeleteProtected(paths, credentialFileName)
}

// SaveLastPhone 记住最近一次短信登录使用的手机号。
func SaveLastPhone(paths storage.Paths, phone string) error {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return errPhoneRequired
	}
	if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		return err
	}
	return storage.SaveProtectedJSON(paths, phoneFileName, phoneRecord{Phone: phone, SavedAt: time.Now()})
}

// LoadLastPhone 读取上次短信登录用的手机号；没有记录时返回空串。
func LoadLastPhone(paths storage.Paths) (string, error) {
	var record phoneRecord
	if err := storage.LoadProtectedJSON(paths, phoneFileName, &record); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(record.Phone), nil
}

// phoneRecord 是手机号的落盘结构。
type phoneRecord struct {
	Phone   string    `json:"phone"`
	SavedAt time.Time `json:"saved_at"`
}

// LoginWithStoredCredentials 用本机保存的账号密码登录，并把新会话写回磁盘。
func LoginWithStoredCredentials(ctx context.Context, paths storage.Paths, options PasswordLoginOptions) (Session, error) {
	credentials, err := LoadCredentials(paths)
	if err != nil {
		return Session{}, err
	}
	options.Username = credentials.Username
	options.Password = credentials.Password
	session, err := PasswordLogin(ctx, options)
	if err != nil {
		return Session{}, err
	}
	if err := SaveSession(paths, session); err != nil {
		return Session{}, err
	}
	return session, nil
}
