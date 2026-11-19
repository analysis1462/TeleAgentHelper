//go:build !windows

package main

import (
	"errors"
	"os"
)

func installAutostart(executable, arguments string) error {
	return errors.New("当前系统不支持开机自启")
}

func removeAutostart() error {
	return errors.New("当前系统不支持开机自启")
}

func readAutostart() (string, bool) {
	return "", false
}

func defaultDaemonExecutable() string {
	current, err := os.Executable()
	if err != nil {
		return ""
	}
	return current
}
