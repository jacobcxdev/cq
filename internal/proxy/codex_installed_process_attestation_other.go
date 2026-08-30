//go:build !darwin && !linux

package proxy

import "context"

type codexInstalledUnsupportedProcessVerifier struct{}

func defaultCodexInstalledProcessPlatformVerifier() codexInstalledProcessPlatformVerifier {
	return codexInstalledUnsupportedProcessVerifier{}
}

func (codexInstalledUnsupportedProcessVerifier) Capture(context.Context) (codexInstalledProcessPlatformProof, error) {
	return codexInstalledProcessPlatformProof{}, errCodexInstalledProcessAttestation
}

func runCodexInstalledVersionCommand(context.Context, string, codexInstalledExecutableProof) ([]byte, error) {
	return nil, errCodexInstalledProcessAttestation
}
