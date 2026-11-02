//go:build darwin

package main

import (
	"context"
	"io"
	"os/exec"

	"github.com/jacobcxdev/cq/internal/nativeusage"
)

func verifyNativeUsageTrust(ctx context.Context, info nativeusage.CertificateInfo) error {
	return verifyNativeUsageTrustWithCommand(ctx, info, func(ctx context.Context, executable string, arguments ...string) error {
		command := exec.CommandContext(ctx, executable, arguments...)
		command.Stdout, command.Stderr = io.Discard, io.Discard
		return command.Run()
	})
}
