//go:build !windows

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "TeleAgent GUI 仅支持 Windows。")
	os.Exit(2)
}
