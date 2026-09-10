package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jacobcxdev/cq/internal/proxy"
)

// isMachineABI recognises only frozen launcher and package-hook grammars. It
// performs no IO: descriptor authority remains the existing handler's job.
// T27 calls dispatchMachineABI before the public parser; malformed forms are
// deliberately left to that parser, which must reject private flags and paths.
func isMachineABI(argv []string) bool {
	if len(argv) < 2 {
		return false
	}
	if argv[0] == "service" {
		for _, arg := range argv[2:] {
			if arg == "--service-executable=" {
				return false
			}
		}
		switch argv[1] {
		case "install", "uninstall":
			// Bare install/uninstall are public v2 commands. Package-only options
			// select the legacy parser, including its omitted-owner behaviour.
			packageOption := false
			for _, arg := range argv[2:] {
				name, _, _ := strings.Cut(arg, "=")
				switch name {
				case "--owner", "--service-executable", "--installer-lock-held":
					packageOption = true
				}
			}
			if !packageOption {
				return false
			}
		case "snapshot", "restore":
		default:
			return false
		}
		command, err := parseServiceCommand(argv[1:])
		return err == nil && command.HelpPath == nil
	}
	if argv[0] != "proxy" {
		return false
	}
	if isLinuxAcceptanceHelperCommand(argv) {
		return true
	}
	if isCandidateRuntimeCommand(argv) {
		_, _, err := parseCandidateRuntimeArguments(argv[2:])
		return err == nil
	}
	if len(argv) == 3 && argv[1] == "hook" && argv[2] == "codex-stop" {
		return true
	}
	if argv[1] != "start" || len(argv) < 3 {
		return false
	}
	if argv[2] == "--runtime-role" {
		_, err := proxy.ParseRuntimeRoleArguments(argv[2:])
		return err == nil
	}
	// The legacy proxy parser also accepts ordinary public options. Only the
	// two exact child pairs belong here; neither pair may repeat or be omitted.
	if len(argv) != 6 || !((argv[2] == "--port" && argv[4] == "--linux-validation-candidate-fd") ||
		(argv[2] == "--linux-validation-candidate-fd" && argv[4] == "--port")) {
		return false
	}
	_, err := parseProxyCommandOptions(argv[2:])
	return err == nil
}

// dispatchMachineABI preserves native stdout and exit-1 error rendering. It is
// intentionally not wired into main until the public parser can reject every
// unhandled internal-looking form without invoking a permissive legacy alias.
func dispatchMachineABI(argv []string) (handled bool, exitCode int) {
	if !isMachineABI(argv) {
		return false, 0
	}
	var err error
	switch {
	case argv[0] == "service":
		err = runService(argv[1:])
	case isLinuxAcceptanceHelperCommand(argv):
		if runLinuxAcceptanceHelper(context.Background()) != nil {
			err = errors.New("Linux acceptance helper failed")
		}
	case isCandidateRuntimeCommand(argv):
		if runCandidateRuntimeChild(context.Background(), argv[2:]) != nil {
			err = errors.New("candidate runtime failed")
		}
	case argv[1] == "start":
		var opts proxyCommandOptions
		opts, err = parseProxyCommandOptions(argv[2:])
		if err == nil {
			err = runProxyStart(opts)
		}
	default:
		fmt.Fprintln(os.Stderr, "cq: warning: Deprecated syntax; use cq codex proxy hook stop.")
		err = runProxyHook(argv[2:], os.Stdin, os.Stdout)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "cq: %v\n", err)
		return true, 1
	}
	return true, 0
}
