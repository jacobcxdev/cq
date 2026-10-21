//go:build !darwin

package dots

import (
	"context"
	"errors"
	"io"
)

func cloudStockExec(string, []string, []string, io.Reader, io.Writer, io.Writer) (bool, error) {
	return false, nil
}

func nativeCloudBridge(context.Context, *Settings, []string, io.Reader, io.Writer, io.Writer) (bool, error) {
	return false, nil
}

func cloudBridgeLocal() (*cloudLocal, error) {
	return nil, errors.New("native bridge handoff requires macOS")
}
