//go:build windows

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jacobcxdev/cq/internal/dots"
)

func runWindowsHarnessStdioEntry(args []string, stdin io.Reader, stdout, stderr io.Writer) (bool, error) {
	stock := os.Getenv(windowsHarnessStockCLIEnv)
	command := args
	for len(command) >= 2 && (command[0] == "-c" || command[0] == "--config") {
		command = command[2:]
	}
	if stock == "" || len(command) == 0 || command[0] != "app-server" {
		return false, nil
	}
	if !filepath.IsAbs(stock) || filepath.Clean(stock) != stock || !strings.EqualFold(filepath.Base(stock), "codex.exe") {
		return true, errors.New("invalid stock Codex CLI path")
	}
	stockInfo, err := os.Lstat(stock)
	if err != nil || !stockInfo.Mode().IsRegular() {
		return true, errors.New("stock Codex CLI unavailable")
	}
	self, err := os.Executable()
	if err != nil {
		return true, errors.New("CQ executable path unavailable")
	}
	selfInfo, err := os.Stat(self)
	if err != nil || os.SameFile(stockInfo, selfInfo) {
		return true, errors.New("stock Codex CLI must differ from CQ")
	}
	if err := os.Unsetenv(windowsHarnessStockCLIEnv); err != nil {
		return true, err
	}
	return true, dots.RunWindowsHarnessStdio(context.Background(), stock, args, os.Environ(), stdin, stdout, stderr)
}
