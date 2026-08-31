//go:build darwin

package proxy

import (
	"context"
	"errors"
	"os"
)

type darwinCodexAcceptanceConfinement struct{}

func defaultCodexAcceptanceConfinement() codexAcceptanceConfinement {
	return darwinCodexAcceptanceConfinement{}
}

func (darwinCodexAcceptanceConfinement) Execute(ctx context.Context, execution codexAcceptanceExecution) ([]byte, error) {
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		return nil, errors.New("Codex acceptance network confinement unavailable")
	}
	profile, err := codexAcceptanceSandboxProfile(execution.command)
	if err != nil {
		return nil, errors.New("Codex acceptance sandbox authority unavailable")
	}
	execution.args = append([]string{"-p", profile, execution.executable}, execution.args...)
	execution.executable = "/usr/bin/sandbox-exec"
	return runCodexAcceptanceExecution(ctx, execution)
}
