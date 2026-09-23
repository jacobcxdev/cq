package main

import (
	"context"
	"errors"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func runCLIV2(ctx context.Context, argv []string, session *cli.Session) int {
	return cli.Run(ctx, argv, session, lookupCLIV2)
}

// Registration is explicit: adding a catalogue leaf requires choosing its owner.
// The integration test compares this exact set with the normative catalogue.
var cliV2Handlers = assembleCLIV2()

func assembleCLIV2() map[string]cli.Handler {
	paths := []string{
		"auth refresh",
		"check",
		"claude account activate",
		"claude account list",
		"claude account login",
		"claude account remove",
		"claude proxy pin clear",
		"claude proxy pin set",
		"claude proxy pin show",
		"codex account activate",
		"codex account list",
		"codex account login",
		"codex account remove",
		"codex proxy canary start",
		"codex proxy canary status",
		"codex proxy canary stop",
		"codex proxy credential-endpoint legacy activate",
		"codex proxy credential-endpoint legacy finalise",
		"codex proxy credential-endpoint legacy inspect",
		"codex proxy credential-endpoint legacy prepare",
		"codex proxy credential-endpoint legacy resume",
		"codex proxy credential-endpoint legacy rollback",
		"codex proxy fallback clear",
		"codex proxy fallback set",
		"codex proxy fallback show",
		"codex proxy fixture create",
		"codex proxy hook stop",
		"codex proxy lease invalidate",
		"codex proxy pin clear",
		"codex proxy pin set",
		"codex proxy pin show",
		"codex proxy policy apply",
		"codex proxy policy show",
		"codex proxy pool rename",
		"codex proxy pool set",
		"codex proxy pool value",
		"codex proxy prime disable",
		"codex proxy prime enable",
		"codex proxy prime status",
		"codex proxy readiness show",
		"codex proxy reserve clear",
		"codex proxy reserve disable",
		"codex proxy reserve enable",
		"codex proxy reserve set",
		"codex proxy reserve status",
		"codex proxy reserve windows",
		"codex proxy session bind",
		"codex proxy session digest",
		"codex proxy session list",
		"codex proxy session show",
		"codex proxy session unbind",
		"codex proxy trace",
		"codex proxy validate http",
		"codex proxy validate websocket",
		"codex reset list",
		"codex reset recommend",
		"codex reset use",
		"completion",
		"gemini account show",
		"help",
		"models list",
		"models overlay add",
		"models overlay prune",
		"models overlay remove",
		"models refresh",
		"proxy candidate client-safety refresh",
		"proxy candidate prepare",
		"proxy candidate receipt show",
		"proxy candidate release activate",
		"proxy candidate release validate",
		"proxy candidate remove",
		"proxy candidate start",
		"proxy candidate status",
		"proxy candidate stop",
		"proxy health",
		"proxy operation status",
		"proxy rescue enter",
		"proxy rescue exit",
		"proxy rescue status",
		"proxy serve",
		"proxy state initialise",
		"proxy status",
		"service install",
		"service restart",
		"service start",
		"service status",
		"service stop",
		"service uninstall",
		"version",
	}
	families := []cli.Lookup{lookupV2Auth, lookupV2AccountInspection, lookupV2AccountMutation, lookupV2Canary, lookupV2Hook, lookupV2Endpoint, lookupV2Candidate, lookupV2Rescue, lookupV2Reserve, lookupV2Service, lookupV2Policy, lookupV2RoutingDiagnostics, lookupV2Models, lookupV2ResetInspection, lookupV2ResetUse, lookupV2Validation, lookupV2Selection, lookupV2Proxy}
	handlers := map[string]cli.Handler{
		"check": handleV2Check,
		// Run owns pure utility execution before operational lookup.
		"help": func(_ context.Context, inv cli.Invocation, _ *cli.Session) cli.Outcome {
			text, _ := cli.Help(inv.Path)
			return cli.Outcome{Human: text}
		},
		"completion": func(_ context.Context, inv cli.Invocation, _ *cli.Session) cli.Outcome {
			return cli.CompletionOutcome(inv.Arguments["shell"][0])
		},
		"version": func(_ context.Context, _ cli.Invocation, s *cli.Session) cli.Outcome {
			return cli.VersionOutcome(s.BuildInfo)
		},
	}
	for _, path := range paths {
		for _, lookup := range families {
			if handler, ok := lookup(path); ok {
				if handlers[path] != nil {
					panic("duplicate CLI v2 registration: " + path)
				}
				handlers[path] = handler
			}
		}
		if handlers[path] == nil {
			panic("missing CLI v2 registration: " + path)
		}
	}
	handlers["auth refresh"] = scheduledV2Refresh(handlers["auth refresh"])
	return handlers
}
func lookupCLIV2(path string) (cli.Handler, bool) {
	handler, ok := cliV2Handlers[path]
	return handler, ok
}

// Native managers validate their owned descriptor, executable and root before
// wrapping the operation. Preserve their waited completion receipt at cutover.
func scheduledV2Refresh(handler cli.Handler) cli.Handler {
	return func(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
		if serviceRefreshRunner == nil {
			return handler(ctx, inv, session)
		}
		var outcome cli.Outcome
		ran := false
		err := serviceRefreshRunner(func() error {
			ran = true
			outcome = handler(ctx, inv, session)
			if outcome.ExitCode != 0 {
				return errors.New("scheduled credential refresh failed")
			}
			return nil
		})
		if err != nil && (!ran || outcome.ExitCode == 0) {
			return authFailure("auth_store_failed", "Cannot record scheduled refresh completion.")
		}
		return outcome
	}
}

// v2EnvironmentFailure preserves the resolver's safe common diagnostic at adapters.
func v2EnvironmentFailure(err error) (cli.Outcome, bool) {
	var environment *userdirs.EnvironmentError
	if errors.As(err, &environment) {
		return cli.Outcome{ExitCode: environment.ExitCode, Errors: []cli.Diagnostic{{Code: environment.Code, Message: environment.Error()}}}, true
	}
	return cli.Outcome{}, false
}
