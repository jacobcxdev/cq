package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jacobcxdev/cq/internal/installstate"
)

type serviceCommand struct {
	Action   string
	Owner    installstate.Owner
	OwnerSet bool
	JSON     bool
	HelpPath []string
}

var serviceLifecycleFactory = func() (*serviceLifecycle, error) {
	return nil, fmt.Errorf("service platform adapter is unavailable")
}

func runService(args []string) error {
	lifecycle, err := serviceLifecycleFactory()
	if err != nil {
		return err
	}
	return runServiceWithLifecycle(args, lifecycle, os.Stdout)
}

func runServiceWithLifecycle(args []string, lifecycle *serviceLifecycle, output io.Writer) error {
	command, err := parseServiceCommand(args)
	if err != nil {
		return err
	}
	if command.HelpPath != nil {
		if output == nil {
			return nil
		}
		return writeManualHelp(output, command.HelpPath)
	}
	if output == nil {
		output = io.Discard
	}

	ctx := context.Background()
	switch command.Action {
	case "install":
		return lifecycle.Install(ctx, command.Owner)
	case "restart":
		return lifecycle.Restart(ctx)
	case "status":
		status, err := lifecycle.Status(ctx)
		if err != nil {
			return err
		}
		if command.JSON {
			encoder := json.NewEncoder(output)
			encoder.SetEscapeHTML(false)
			return encoder.Encode(status)
		}
		return writeServiceStatus(output, status)
	case "uninstall":
		return lifecycle.Uninstall(ctx, command.Owner)
	default:
		return fmt.Errorf("unknown service action %q", command.Action)
	}
}

func parseServiceCommand(args []string) (serviceCommand, error) {
	if len(args) == 0 {
		return serviceCommand{}, fmt.Errorf("missing service subcommand")
	}
	if args[0] == "--help" || args[0] == "-h" {
		return serviceCommand{HelpPath: []string{"service"}}, nil
	}
	if args[0] == "help" {
		path := append([]string{"service"}, args[1:]...)
		if _, ok := manualHelp(path); !ok {
			return serviceCommand{}, fmt.Errorf("no help for command path: %s", strings.Join(path, " "))
		}
		return serviceCommand{HelpPath: path}, nil
	}

	command := serviceCommand{Action: args[0], Owner: installstate.OwnerManual}
	switch command.Action {
	case "install", "restart", "status", "uninstall":
	default:
		return serviceCommand{}, fmt.Errorf("unknown service command: %s", command.Action)
	}
	for index := 1; index < len(args); index++ {
		argument := args[index]
		switch {
		case argument == "--help" || argument == "-h":
			if index != len(args)-1 {
				return serviceCommand{}, fmt.Errorf("service %s: unexpected arguments after help", command.Action)
			}
			command.HelpPath = []string{"service", command.Action}
			return command, nil
		case argument == "--json":
			if command.JSON {
				return serviceCommand{}, fmt.Errorf("service %s: duplicate --json", command.Action)
			}
			command.JSON = true
		case argument == "--owner":
			if index+1 >= len(args) {
				return serviceCommand{}, fmt.Errorf("service %s: --owner requires a value", command.Action)
			}
			index++
			if err := setServiceOwner(&command, args[index]); err != nil {
				return serviceCommand{}, err
			}
		case strings.HasPrefix(argument, "--owner="):
			if err := setServiceOwner(&command, strings.TrimPrefix(argument, "--owner=")); err != nil {
				return serviceCommand{}, err
			}
		default:
			return serviceCommand{}, fmt.Errorf("service %s: unexpected argument %q", command.Action, argument)
		}
	}
	if command.JSON && command.Action != "status" {
		return serviceCommand{}, fmt.Errorf("service %s: --json is only valid with status", command.Action)
	}
	if command.OwnerSet && command.Action != "install" && command.Action != "uninstall" {
		return serviceCommand{}, fmt.Errorf("service %s: --owner is only valid with install or uninstall", command.Action)
	}
	return command, nil
}

func setServiceOwner(command *serviceCommand, value string) error {
	if command.OwnerSet {
		return fmt.Errorf("service %s: duplicate --owner", command.Action)
	}
	owner := installstate.Owner(value)
	if owner != installstate.OwnerHomebrew && owner != installstate.OwnerWinGet && owner != installstate.OwnerGo {
		return fmt.Errorf("service %s: invalid package owner %q", command.Action, value)
	}
	command.Owner = owner
	command.OwnerSet = true
	return nil
}

func writeServiceStatus(output io.Writer, status serviceStatus) error {
	overall := "degraded"
	if status.Proxy.Healthy && status.Refresh.Healthy {
		overall = "healthy"
	}
	_, err := fmt.Fprintf(
		output,
		"CQ services: %s\nProxy: %s (%s)\nRefresh: %s (%s)\n",
		overall,
		componentHealth(status.Proxy),
		status.Proxy.ID,
		componentHealth(status.Refresh),
		status.Refresh.ID,
	)
	return err
}

func componentHealth(status componentStatus) string {
	if status.Healthy {
		return "healthy"
	}
	if status.Registered {
		return "unhealthy"
	}
	return "not installed"
}
