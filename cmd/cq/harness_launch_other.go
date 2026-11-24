//go:build !windows

package main

import (
	"context"
	"errors"
	"io"
)

func launchHarnessDesktop(context.Context) error {
	return errors.New("the harness launch command requires Windows")
}

func runWindowsHarnessStdioEntry([]string, io.Reader, io.Writer, io.Writer) (bool, error) {
	return false, nil
}
