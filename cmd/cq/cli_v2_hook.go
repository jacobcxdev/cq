package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func lookupV2Hook(path string) (cli.Handler, bool) {
	if path == "codex proxy hook stop" {
		return handleV2Hook, true
	}
	return nil, false
}

func handleV2Hook(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
	return handleV2HookWithPreparation(ctx, inv, session, func(context.Context) (proxyCodexHookDependencies, error) {
		return proxyCodexHookDependencies{LoadConfig: proxy.LoadExistingConfig, Doer: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("Codex turn receipt redirect refused") }}}, nil
	})
}

func handleV2HookWithPreparation(parent context.Context, _ cli.Invocation, session *cli.Session, prepare func(context.Context) (proxyCodexHookDependencies, error)) cli.Outcome {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	var output bytes.Buffer
	deps, err := prepare(ctx)
	if err == nil && ctx.Err() == nil {
		err = runProxyCodexStopHook(ctx, session.In, &output, deps)
	}
	switch {
	case errors.Is(parent.Err(), context.Canceled), errors.Is(err, context.Canceled):
		return v2SelectionFailure(130, "interrupted", "Operation interrupted; inspect state before retrying.")
	case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		return v2SelectionFailure(7, "hook_timeout", "The Codex turn receipt lookup timed out.")
	case errors.Is(err, errCodexStopHookInput):
		return v2SelectionFailure(2, "hook_input_invalid", "Invalid Codex Stop hook input.")
	case errors.Is(err, errCodexStopHookAuth):
		return v2SelectionFailure(5, "hook_auth_failed", "The Codex turn receipt lookup was not authorised.")
	case err != nil:
		return v2SelectionFailure(4, "hook_unavailable", "The Codex turn receipt lookup is unavailable.")
	}
	return cli.Outcome{Data: bytes.TrimSpace(output.Bytes())}
}
