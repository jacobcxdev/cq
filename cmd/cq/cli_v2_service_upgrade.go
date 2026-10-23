package main

import (
	"context"
	"fmt"
	"io"

	"github.com/jacobcxdev/cq/internal/cli"
)

// Upgrade is public, but its released native receipt protocol is also consumed
// by package hooks. Both dispatcher paths therefore use the same lifecycle
// runner and own their output instead of adding a CLI-v2 envelope.
func handleV2ServiceUpgrade(_ context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
	return handleV2ServiceUpgradeWithRunner(inv, session, func(args []string, output io.Writer) error {
		command, err := parseServiceCommand(args)
		if err != nil {
			return err
		}
		lifecycle, err := serviceLifecycleFactory(command.ServiceExecutable)
		if err != nil {
			return err
		}
		return runServiceWithLifecycleInput(args, lifecycle, output, nil)
	})
}

func handleV2ServiceUpgradeWithRunner(inv cli.Invocation, session *cli.Session, run func([]string, io.Writer) error) cli.Outcome {
	args := []string{"upgrade", "--owner=" + inv.Options["owner"][0], "--candidate-executable=" + inv.Options["candidate-executable"][0]}
	if values := inv.Options["service-executable"]; len(values) > 0 {
		args = append(args, "--service-executable="+values[0])
	}
	if inv.JSON {
		args = append(args, "--json")
	}
	outcome := cli.Outcome{Streamed: true}
	if err := run(args, session.Out); err != nil {
		outcome.ExitCode = 1
		_, _ = fmt.Fprintf(session.Err, "cq: %v\n", err)
	}
	return outcome
}
