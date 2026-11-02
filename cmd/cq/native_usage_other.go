//go:build !darwin

package main

import (
	"context"
	"errors"

	"github.com/jacobcxdev/cq/internal/nativeusage"
)

func verifyNativeUsageTrust(context.Context, nativeusage.CertificateInfo) error {
	return errors.New("native ChatGPT usage certificate trust checks are only supported on macOS")
}
