//go:build !darwin && !linux

package proxy

import (
	"context"
	"errors"
)

type unsupportedCodexAcceptanceConfinement struct{}

func defaultCodexAcceptanceConfinement() codexAcceptanceConfinement {
	return unsupportedCodexAcceptanceConfinement{}
}

func (unsupportedCodexAcceptanceConfinement) Execute(context.Context, codexAcceptanceExecution) ([]byte, error) {
	return nil, errors.New("Codex acceptance network confinement unavailable")
}
