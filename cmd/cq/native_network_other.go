//go:build !darwin

package main

import (
	"context"
	"errors"
)

func defaultNativeNetworkRunner() nativeNetworkRunner {
	unsupported := func(context.Context) error {
		return errors.New("native ChatGPT network routing is only supported on macOS")
	}
	return nativeNetworkRunner{
		validateBroker: unsupported,
		verifyHost:     unsupported,
	}
}
