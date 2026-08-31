package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jacobcxdev/cq/internal/installstate"
)

type serviceCommand struct {
	Action            string
	Owner             installstate.Owner
	OwnerSet          bool
	JSON              bool
	ServiceExecutable string
	SnapshotFile      string
	InstallerLockHeld bool
	HelpPath          []string
}

var serviceLifecycleFactory = func(string) (*serviceLifecycle, error) {
	return nil, fmt.Errorf("service platform adapter is unavailable")
}

func runService(args []string) error {
	command, err := parseServiceCommand(args)
	if err != nil {
		return err
	}
	lifecycle, err := serviceLifecycleFactory(command.ServiceExecutable)
	if err != nil {
		return err
	}
	return runServiceWithLifecycle(args, lifecycle, os.Stdout)
}

func runServiceWithLifecycle(args []string, lifecycle *serviceLifecycle, output io.Writer) (returnErr error) {
	return runServiceWithLifecycleInput(args, lifecycle, output, os.Stdin)
}

func runServiceWithLifecycleInput(args []string, lifecycle *serviceLifecycle, output io.Writer, inheritedLock *os.File) (returnErr error) {
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
	if command.Action != "status" && (lifecycle == nil || lifecycle.MutationLocker == nil) {
		return fmt.Errorf("service mutation lock is unavailable")
	}
	if command.Action != "status" && command.InstallerLockHeld {
		validator, ok := lifecycle.MutationLocker.(interface {
			ValidateInherited(*os.File) error
		})
		if !ok {
			return fmt.Errorf("inherited installer lock validation is unavailable")
		}
		if err := validator.ValidateInherited(inheritedLock); err != nil {
			return fmt.Errorf("validate inherited installer lock: %w", err)
		}
	}
	if command.Action != "status" && !command.InstallerLockHeld {
		lock, err := lifecycle.MutationLocker.Acquire()
		if err != nil {
			return err
		}
		defer func() { returnErr = errors.Join(returnErr, lock.Close()) }()
	}

	ctx := context.Background()
	switch command.Action {
	case "install":
		return lifecycle.Install(ctx, command.Owner)
	case "restart":
		return lifecycle.Restart(ctx)
	case "snapshot":
		return lifecycle.Snapshot(ctx, command.Owner, command.SnapshotFile)
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
	case "restore":
		return lifecycle.Restore(ctx, command.Owner, command.SnapshotFile)
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
	case "install", "restart", "snapshot", "status", "uninstall", "restore":
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
		case strings.HasPrefix(argument, "--service-executable="):
			if command.ServiceExecutable != "" {
				return serviceCommand{}, fmt.Errorf("service %s: duplicate service executable", command.Action)
			}
			command.ServiceExecutable = strings.TrimPrefix(argument, "--service-executable=")
		case strings.HasPrefix(argument, "--snapshot-file="):
			if command.SnapshotFile != "" {
				return serviceCommand{}, fmt.Errorf("service %s: duplicate service snapshot file", command.Action)
			}
			command.SnapshotFile = strings.TrimPrefix(argument, "--snapshot-file=")
		case argument == "--installer-lock-held":
			if command.InstallerLockHeld {
				return serviceCommand{}, fmt.Errorf("service %s: duplicate installer lock marker", command.Action)
			}
			command.InstallerLockHeld = true
		default:
			return serviceCommand{}, fmt.Errorf("service %s: unexpected argument %q", command.Action, argument)
		}
	}
	if command.JSON && command.Action != "status" {
		return serviceCommand{}, fmt.Errorf("service %s: --json is only valid with status", command.Action)
	}
	packageAction := command.Action == "install" || command.Action == "uninstall" || command.Action == "snapshot" || command.Action == "restore"
	if command.OwnerSet && !packageAction {
		return serviceCommand{}, fmt.Errorf("service %s: --owner is only valid with a package lifecycle action", command.Action)
	}
	if command.ServiceExecutable != "" {
		if command.Owner != installstate.OwnerHomebrew || (command.Action != "install" && command.Action != "uninstall") {
			return serviceCommand{}, fmt.Errorf("service %s: service executable is only valid for Homebrew lifecycle hooks", command.Action)
		}
		if !filepath.IsAbs(command.ServiceExecutable) || filepath.Clean(command.ServiceExecutable) != command.ServiceExecutable {
			return serviceCommand{}, fmt.Errorf("service %s: service executable must be a clean absolute path", command.Action)
		}
	}
	if command.SnapshotFile != "" {
		if (command.Action != "snapshot" && command.Action != "restore") || !filepath.IsAbs(command.SnapshotFile) || filepath.Clean(command.SnapshotFile) != command.SnapshotFile {
			return serviceCommand{}, fmt.Errorf("service %s: service snapshot file must be a clean absolute path", command.Action)
		}
	} else if command.Action == "snapshot" || command.Action == "restore" {
		return serviceCommand{}, fmt.Errorf("service %s: service snapshot file is required", command.Action)
	}
	if command.InstallerLockHeld && (!command.OwnerSet || command.Owner == installstate.OwnerManual || !packageAction) {
		return serviceCommand{}, fmt.Errorf("service %s: installer lock marker requires a package lifecycle action", command.Action)
	}
	if (command.Action == "snapshot" || command.Action == "restore") && !command.InstallerLockHeld {
		return serviceCommand{}, fmt.Errorf("service %s: installer lock marker is required", command.Action)
	}
	return command, nil
}

func resolveServiceExecutable(stable string) (string, error) {
	current, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve current executable: %w", err)
	}
	current, err = filepath.Abs(current)
	if err != nil {
		return "", fmt.Errorf("resolve absolute executable: %w", err)
	}
	current = filepath.Clean(current)
	if stable == "" {
		return current, nil
	}
	if !filepath.IsAbs(stable) || filepath.Clean(stable) != stable {
		return "", fmt.Errorf("service executable must be a clean absolute path")
	}
	currentInfo, err := os.Stat(current)
	if err != nil {
		return "", fmt.Errorf("inspect current executable: %w", err)
	}
	stableInfo, err := os.Stat(stable)
	if err != nil {
		return "", fmt.Errorf("inspect stable executable: %w", err)
	}
	if !os.SameFile(currentInfo, stableInfo) {
		return "", fmt.Errorf("%w: stable executable differs from current process", installstate.ErrOwnershipConflict)
	}
	return stable, nil
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
