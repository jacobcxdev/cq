//go:build windows

package main

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/jacobcxdev/cq/internal/installer"
	"github.com/jacobcxdev/cq/internal/installstate"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func resolvePlatformInstallation(ctx context.Context, owner installstate.Owner, _ userdirs.Roots, sourceInstaller string) (platformInstallation, error) {
	services := []string{`\cq\Proxy`, `\cq\Refresh`}
	switch owner {
	case installstate.OwnerGo:
		executable, err := installer.DefaultGoDestinationResolver().Resolve(ctx)
		if err != nil {
			return platformInstallation{}, err
		}
		return platformInstallation{Executable: executable, Services: services, Metadata: noPlatformMetadata{}}, nil
	case installstate.OwnerWinGet:
		anchors, err := userdirs.WindowsAppDataAnchors()
		if err != nil {
			return platformInstallation{}, err
		}
		root, err := installer.WindowsInstallRoot(anchors.LocalAppData)
		if err != nil {
			return platformInstallation{}, err
		}
		executable := filepath.Join(root, "cq.exe")
		return platformInstallation{
			Executable: executable,
			Services:   services,
			Metadata:   installer.NewWindowsMetadata(root, sourceInstaller),
		}, nil
	default:
		return platformInstallation{}, fmt.Errorf("unsupported installer owner on Windows")
	}
}
