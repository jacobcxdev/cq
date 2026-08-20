//go:build windows

package main

import (
	"fmt"
	"io"
)

func buildOperationalRelease(manifest releaseBuildManifestV1, output io.Writer) error {
	_ = manifest
	_ = output
	return fmt.Errorf("release building is supported only on the pinned Darwin toolchain")
}
