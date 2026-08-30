//go:build darwin || linux

package main

import (
	"context"
	"fmt"
	"runtime"

	"github.com/jacobcxdev/cq/internal/installer"
	"github.com/jacobcxdev/cq/internal/installstate"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func resolvePlatformInstallation(ctx context.Context, owner installstate.Owner, _ userdirs.Roots, _ string) (platformInstallation, error) {
	if owner != installstate.OwnerGo {
		return platformInstallation{}, fmt.Errorf("unsupported installer owner on this platform")
	}
	executable, err := installer.DefaultGoDestinationResolver().Resolve(ctx)
	if err != nil {
		return platformInstallation{}, err
	}
	services := []string{"cq-proxy.service", "cq-refresh.timer"}
	if runtime.GOOS == "darwin" {
		services = []string{"dev.jacobcx.cq.proxy", "dev.jacobcx.cq.refresh"}
	}
	return platformInstallation{Executable: executable, Services: services, Metadata: noPlatformMetadata{}}, nil
}
