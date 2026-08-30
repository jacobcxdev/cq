//go:build linux

package main

import (
	"context"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func isLinuxAcceptanceHelperCommand(args []string) bool {
	return len(args) == 2 && args[0] == "proxy" && args[1] == "__linux-acceptance-helper"
}

func runLinuxAcceptanceHelper(ctx context.Context) error {
	return proxy.RunLinuxAcceptanceNamespaceHelper(ctx)
}
