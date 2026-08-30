//go:build linux

package proxy

import (
	"context"
	"errors"
)

type linuxCodexAcceptanceConfinement struct{}

func defaultCodexAcceptanceConfinement() codexAcceptanceConfinement {
	return linuxCodexAcceptanceConfinement{}
}

func (linuxCodexAcceptanceConfinement) Execute(context.Context, codexAcceptanceExecution) ([]byte, error) {
	return nil, errors.New("Codex acceptance network confinement unavailable")
}
