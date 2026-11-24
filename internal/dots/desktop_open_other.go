//go:build !windows

package dots

import (
	"context"
	"os/exec"
)

func openDesktopApp(ctx context.Context, app string) error {
	return exec.CommandContext(ctx, "/usr/bin/open", "-a", app).Run()
}
