//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const autostartKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const autostartName = "CtyunHelper-TeleAgent"

func installAutostart(executable, arguments string) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, autostartKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开注册表启动项: %w", err)
	}
	defer key.Close()
	command := "\"" + executable + "\""
	if strings.TrimSpace(arguments) != "" {
		command += " " + arguments
	}
	if err := key.SetStringValue(autostartName, command); err != nil {
		return fmt.Errorf("写入注册表启动项: %w", err)
	}
	return nil
}

func removeAutostart() error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, autostartKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开注册表启动项: %w", err)
	}
	defer key.Close()
	if err := key.DeleteValue(autostartName); err != nil && err != registry.ErrNotExist {
		return fmt.Errorf("删除注册表启动项: %w", err)
	}
	return nil
}

func readAutostart() (string, bool) {
	key, err := registry.OpenKey(registry.CURRENT_USER, autostartKey, registry.QUERY_VALUE)
	if err != nil {
		return "", false
	}
	defer key.Close()
	value, _, err := key.GetStringValue(autostartName)
	if err != nil {
		return "", false
	}
	return value, true
}

// defaultDaemonExecutable 优先使用同目录下无控制台的 teleagent-daemon.exe。
func defaultDaemonExecutable() string {
	current, err := os.Executable()
	if err != nil {
		return ""
	}
	dir := filepath.Dir(current)
	candidate := filepath.Join(dir, "teleagent-daemon.exe")
	if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
		return candidate
	}
	return current
}
