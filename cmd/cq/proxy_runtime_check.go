package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/jacobcxdev/cq/internal/installer"
	"github.com/jacobcxdev/cq/internal/proxy"
)

// Preflight never creates config, opens the coordinator or sends upstream traffic.
func writeProxyRuntimeCheck(output io.Writer) error {
	if _, err := proxy.LoadExistingConfig(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return json.NewEncoder(output).Encode(installer.RuntimeCheckV1{SchemaVersion: 1, Version: version, ProtocolVersion: installer.RuntimeUpgradeProtocolVersion, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH})
}

func runProxyRuntimeCheck(args []string, output io.Writer) error {
	if len(args) != 1 || args[0] != "--json" {
		return fmt.Errorf("runtime-check requires --json")
	}
	return writeProxyRuntimeCheck(output)
}
