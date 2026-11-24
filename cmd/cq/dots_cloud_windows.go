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

func isWindowsDotsCloudHelper(self, root string, args []string) bool {
	return filepath.IsAbs(root) && filepath.Clean(root) == root &&
		strings.EqualFold(filepath.Clean(self), dots.WindowsCloudWrapperPath(root)) &&
		(len(args) == 0 || args[0] != "__dots")
}

func runWindowsDotsCloudEntry(args []string, stdin io.Reader, stdout, stderr io.Writer) (bool, error) {
	root := os.Getenv(dots.WindowsCloudRootEnv)
	if root == "" {
		return false, nil
	}
	self, err := os.Executable()
	if err != nil {
		return true, errors.New("Dots cloud helper executable path unavailable")
	}
	if !isWindowsDotsCloudHelper(self, root, args) {
		return false, nil
	}
	_, stock, err := resolveWindowsHarnessDesktop()
	if err != nil {
		return true, err
	}
	return true, dots.RunWindowsDotsCloudStdio(context.Background(), root, stock, args, stdin, stdout, stderr)
}
