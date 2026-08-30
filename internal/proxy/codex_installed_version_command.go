package proxy

import (
	"context"
	"os"
	"path/filepath"
)

type codexInstalledVersionRunner interface {
	Run(context.Context, codexAcceptanceCommand) ([]byte, error)
}

func runCodexInstalledVersionCommandWithRunner(
	ctx context.Context,
	path string,
	expected codexInstalledExecutableProof,
	runner codexInstalledVersionRunner,
) ([]byte, error) {
	if ctx == nil || ctx.Err() != nil || path != expected.path || !expected.valid() || !filepath.IsAbs(path) || runner == nil {
		return nil, codexInstalledAttestationError(ctx)
	}
	commandCtx, cancel := context.WithTimeout(ctx, codexInstalledProcessProofTimeout)
	defer cancel()
	shortTempRoot, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		return nil, codexInstalledAttestationError(ctx)
	}
	root, err := os.MkdirTemp(shortTempRoot, codexInstalledHTTPClientTempPrefix)
	if err != nil {
		return nil, codexInstalledAttestationError(ctx)
	}
	defer func() { _ = removeCodexInstalledHTTPClientTempRoot(root) }()
	output, err := runner.Run(commandCtx, codexAcceptanceCommand{
		executable:         path,
		expectedExecutable: expected,
		args:               []string{"--version"},
		env:                codexAcceptanceBaseEnvironment("", "", "", "", ""),
		sandboxWriteRoot:   root,
		captureOutput:      true,
		loopbackOnly:       true,
	})
	if err != nil || len(output) == 0 || len(output) > codexInstalledVersionOutputMaxBytes {
		clearBytes(output)
		return nil, codexInstalledAttestationError(ctx)
	}
	return output, nil
}
