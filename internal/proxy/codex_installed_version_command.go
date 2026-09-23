package proxy

import (
	"context"
	"errors"
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
	return runCodexInstalledVersionCommandWithCleanup(ctx, nil, path, expected, runner)
}
func runCodexInstalledVersionCommandWithCleanup(ctx, cleanup context.Context, path string, expected codexInstalledExecutableProof, runner codexInstalledVersionRunner) (output []byte, returnErr error) {
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
	defer func() { returnErr = errors.Join(returnErr, removeCodexInstalledHTTPClientTempRoot(root)) }()
	output, err = runner.Run(commandCtx, codexAcceptanceCommand{
		cleanupContext:     cleanup,
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
